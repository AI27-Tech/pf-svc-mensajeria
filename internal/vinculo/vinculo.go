// Package vinculo es dueño del vínculo pegajoso Ejecutivo↔cliente (DEC-103 #2 y
// #3, DEC-128): quién es el dueño de cada empresa cliente, la última actividad
// de cada Ejecutivo en mensajeria y a quién va dirigida una conversación nueva.
//
// Disponible = actividad en mensajeria dentro de la Ventana (N, 30 minutos por
// defecto). Una conversación nueva de un cliente con dueño disponible nace
// dirigida a él (ruta_conversacion.dirigida_a): solo él la ve en su bandeja y
// solo él la reclama. Supervisor y Coordinador la leen igual (DEC-131).
//
// Vencimiento sin job: la dirección se evalúa al leer la bandeja y al reclamar
// (CondicionDirigida). Sigue vigente mientras la conversación no tenga reclamo y
// el dueño esté disponible SIN HUECOS desde que la conversación nació. Para eso
// actividad_ejecutivo.disponible_desde guarda el inicio de la racha actual: cada
// actividad que llega después de un hueco mayor a N la reinicia. Así, una
// dirección vencida no revive si el dueño vuelve: la conversación ya es del pool.
//
// Todo se escribe en la transacción de la acción que lo produce (Tomar en el
// reclamo, Actividad en envío, atendida y recibos, Dirigir en la creación).
package vinculo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// VentanaPorDefecto es N cuando la configuración no lo fija (DEC-128 #4).
const VentanaPorDefecto = 30 * time.Minute

// Ventana es N: cuánto tiempo sin actividad deja de estar disponible un
// Ejecutivo. Cero o negativa vale VentanaPorDefecto.
type Ventana time.Duration

// Segundos es el valor que reciben las consultas en make_interval(secs => ...).
func (v Ventana) Segundos() float64 {
	if v <= 0 {
		return VentanaPorDefecto.Seconds()
	}
	return time.Duration(v).Seconds()
}

// Ejecutor es lo que estas funciones necesitan de la transacción del llamador.
// pgx.Tx lo satisface.
type Ejecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CondicionDirigida es la condición SQL "la ruta r sigue dirigida a
// r.dirigida_a": tiene dirigida_a, la conversación nunca se reclamó y el dueño
// está disponible sin huecos desde que la ruta nació. ventana es el placeholder
// del parámetro con Ventana.Segundos() (p. ej. "$3").
func CondicionDirigida(ventana string) string {
	return `(r.dirigida_a IS NOT NULL
	   AND NOT EXISTS (SELECT 1 FROM mensajeria.reclamo h
	                    WHERE h.conversacion_id = r.conversacion_id AND h.tenant_id = r.tenant_id)
	   AND EXISTS (SELECT 1 FROM mensajeria.actividad_ejecutivo a
	                WHERE a.ejecutivo_id = r.dirigida_a
	                  AND a.ultima_en > now() - make_interval(secs => ` + ventana + `)
	                  AND a.disponible_desde <= r.creado_en))`
}

// CondicionVisible es la condición SQL "el lector ve la ruta r": no está
// dirigida a otro Ejecutivo. Un lector vacío (Supervisor, Coordinador) ve todo.
// lector y ventana son placeholders de parámetros (texto y segundos).
func CondicionVisible(lector, ventana string) string {
	return `(NULLIF(` + lector + `::text, '') IS NULL
	   OR r.dirigida_a IS NULL
	   OR r.dirigida_a::text = lower(` + lector + `::text)
	   OR NOT ` + CondicionDirigida(ventana) + `)`
}

// Dirigir dirige la conversación recién enrutada al dueño de su cliente, si el
// dueño está disponible. Si no hay dueño o no está disponible, la ruta queda en
// el pool (dirigida_a NULL). Va en la transacción de la creación.
func Dirigir(ctx context.Context, tx Ejecutor, tenantID, conversacionID string, v Ventana) error {
	if _, err := tx.Exec(ctx,
		`UPDATE mensajeria.ruta_conversacion r
		    SET dirigida_a = d.ejecutivo_id
		   FROM mensajeria.conversacion c
		   JOIN mensajeria.dueno_cliente d
		     ON d.tenant_id = c.tenant_id AND d.cliente_id = c.cliente_id
		   JOIN mensajeria.actividad_ejecutivo a
		     ON a.ejecutivo_id = d.ejecutivo_id
		  WHERE r.conversacion_id = $1 AND r.tenant_id = $2 AND r.activa
		    AND c.id = r.conversacion_id AND c.tenant_id = r.tenant_id
		    AND a.ultima_en > now() - make_interval(secs => $3)`,
		conversacionID, tenantID, v.Segundos(),
	); err != nil {
		return fmt.Errorf("vinculo: dirigir conversación al dueño: %w", err)
	}
	return nil
}

// DirigidaAOtro devuelve el Ejecutivo al que la conversación sigue dirigida, si
// no es actor; vacío si está en el pool o dirigida a actor. Lo usa el reclamo:
// mientras la dirección está vigente, solo el dueño la reclama.
func DirigidaAOtro(ctx context.Context, tx Ejecutor, tenantID, conversacionID, actor string, v Ventana) (string, error) {
	var dueno string
	err := tx.QueryRow(ctx,
		`SELECT r.dirigida_a::text FROM mensajeria.ruta_conversacion r
		  WHERE r.conversacion_id = $1 AND r.tenant_id = $2 AND r.activa
		    AND r.dirigida_a <> $3::uuid
		    AND `+CondicionDirigida("$4"),
		conversacionID, tenantID, actor, v.Segundos(),
	).Scan(&dueno)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("vinculo: leer dirección de la conversación: %w", err)
	}
	return dueno, nil
}

// Toma es el efecto de un reclamo sobre el dueño del cliente.
type Toma struct {
	// Nuevo dice si el reclamante quedó como dueño con este reclamo.
	Nuevo bool
	// Anterior es el dueño que reemplazó; vacío si el cliente no tenía.
	Anterior string
}

// Tomar registra, en la transacción del reclamo ganado, la actividad del
// Ejecutivo y su efecto sobre el dueño del cliente de la conversación: si el
// cliente no tiene dueño, el reclamante lo es (DEC-103 #2); si el dueño no está
// disponible, el reclamante lo reemplaza (DEC-103 #3); si el dueño está
// disponible, no cambia.
//
// "No disponible" exige también que el dueño no lo sea desde hace menos de N
// (d.desde): el ON CONFLICT relee la fila del dueño que otra transacción acaba
// de comprometer, pero no su actividad, así que dos primeros reclamos
// concurrentes no se pisan.
func Tomar(ctx context.Context, tx Ejecutor, tenantID, conversacionID, ejecutivoID string, v Ventana) (Toma, error) {
	var anterior *string
	err := tx.QueryRow(ctx,
		`WITH previo AS (
		   SELECT d.ejecutivo_id::text AS id
		     FROM mensajeria.dueno_cliente d
		     JOIN mensajeria.conversacion c
		       ON c.tenant_id = d.tenant_id AND c.cliente_id = d.cliente_id
		    WHERE c.id = $1 AND c.tenant_id = $2)
		 INSERT INTO mensajeria.dueno_cliente AS d (tenant_id, cliente_id, ejecutivo_id, conversacion_id)
		 SELECT c.tenant_id, c.cliente_id, $3::uuid, c.id
		   FROM mensajeria.conversacion c
		  WHERE c.id = $1 AND c.tenant_id = $2 AND c.cliente_id IS NOT NULL
		 ON CONFLICT (tenant_id, cliente_id) DO UPDATE
		    SET ejecutivo_id = EXCLUDED.ejecutivo_id,
		        conversacion_id = EXCLUDED.conversacion_id,
		        desde = now()
		  WHERE d.ejecutivo_id <> EXCLUDED.ejecutivo_id
		    AND d.desde <= now() - make_interval(secs => $4)
		    AND NOT EXISTS (SELECT 1 FROM mensajeria.actividad_ejecutivo a
		                     WHERE a.ejecutivo_id = d.ejecutivo_id
		                       AND a.ultima_en > now() - make_interval(secs => $4))
		 RETURNING (SELECT id FROM previo)`,
		conversacionID, tenantID, ejecutivoID, v.Segundos(),
	).Scan(&anterior)
	toma := Toma{Nuevo: err == nil}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Toma{}, fmt.Errorf("vinculo: registrar dueño del cliente: %w", err)
	}
	if anterior != nil {
		toma.Anterior = *anterior
	}

	// La actividad va después del dueño: el reclamante nunca cuenta como el dueño
	// disponible que lo bloquearía.
	if _, err := tx.Exec(ctx,
		`INSERT INTO mensajeria.actividad_ejecutivo AS a (ejecutivo_id) VALUES ($1::uuid)
		 ON CONFLICT (ejecutivo_id) DO UPDATE
		    SET disponible_desde = CASE WHEN a.ultima_en > now() - make_interval(secs => $2)
		                                THEN a.disponible_desde ELSE now() END,
		        ultima_en = now()`,
		ejecutivoID, v.Segundos(),
	); err != nil {
		return Toma{}, fmt.Errorf("vinculo: registrar actividad del reclamo: %w", err)
	}
	return toma, nil
}

// Actividad registra la actividad de un sujeto en la transacción de su acción
// (enviar, marcar atendida, recibo). Solo actualiza a quien ya reclamó alguna
// vez: un dueño siempre reclamó (Tomar crea su fila), así que para un cliente o
// un Ejecutivo que nunca reclamó es un no-op sin efecto sobre la disponibilidad.
func Actividad(ctx context.Context, tx Ejecutor, sujetoID string, v Ventana) error {
	if _, err := tx.Exec(ctx,
		`UPDATE mensajeria.actividad_ejecutivo a
		    SET disponible_desde = CASE WHEN a.ultima_en > now() - make_interval(secs => $2)
		                                THEN a.disponible_desde ELSE now() END,
		        ultima_en = now()
		  WHERE a.ejecutivo_id = $1::uuid`,
		sujetoID, v.Segundos(),
	); err != nil {
		return fmt.Errorf("vinculo: registrar actividad: %w", err)
	}
	return nil
}
