package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/jackc/pgx/v5/pgconn"
)

// Eventos de identidad que alimentan mensajeria.rol_sujeto, el rol vigente de
// cada sujeto. Los tres primeros ya figuran en consumidores.TiposIdentidadContratados;
// sujeto.reactivado es el hecho propio de la reactivación (contrato tracer.7).
const (
	TipoRolAsignado       = "rol.asignado"
	TipoRolRevocado       = "rol.revocado"
	TipoSujetoDesactivado = "sujeto.desactivado"
	TipoSujetoReactivado  = "sujeto.reactivado"
)

// TiposRol son los eventos que consume ConsumidorRoles.
var TiposRol = []string{TipoRolAsignado, TipoRolRevocado, TipoSujetoDesactivado, TipoSujetoReactivado}

// Ejecutor es lo que la proyección necesita del pool de chat.
type Ejecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// payloadRol es la vista del cuerpo JSON (nombres del .proto, tiempos en RFC 3339):
// el instante del hecho llega con un nombre distinto por evento.
type payloadRol struct {
	SujetoID      string    `json:"sujeto_id"`
	IdpSubject    string    `json:"idp_subject"` // `sub` del token; vacío en eventos de antes de tracer.46
	Rol           string    `json:"rol"`
	AsignadoEn    time.Time `json:"asignado_en"`
	RevocadoEn    time.Time `json:"revocado_en"`
	DesactivadoEn time.Time `json:"desactivado_en"`
	ReactivadoEn  time.Time `json:"reactivado_en"`
}

// ConsumidorRoles mantiene mensajeria.rol_sujeto. Idempotente por orden de
// tiempo, sin inbox_evento: un evento más viejo que el aplicado no cambia nada.
type ConsumidorRoles struct{ db Ejecutor }

// NuevoConsumidorRoles construye el consumidor sobre el pool de chat.
func NuevoConsumidorRoles(db Ejecutor) *ConsumidorRoles { return &ConsumidorRoles{db: db} }

// Cada hecho es UNA sentencia que también registra el idp_subject, así la fila nunca
// nace sin la marca de tiempo del hecho (si naciera aparte, la lápida de un
// rol.revocado fuera de orden se perdería). La guarda de tiempo decide solo las
// columnas del hecho; el idp_subject es un atributo fijo del sujeto y se escribe
// aunque el hecho sea viejo. $4 vacío no escribe ni borra uno ya conocido.
const (
	sqlAsignado = `INSERT INTO mensajeria.rol_sujeto (sujeto_id, rol, rol_en, idp_subject) VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (sujeto_id) DO UPDATE SET
		 rol = CASE WHEN rol_sujeto.rol_en IS NULL OR rol_sujeto.rol_en <= EXCLUDED.rol_en THEN EXCLUDED.rol ELSE rol_sujeto.rol END,
		 rol_en = CASE WHEN rol_sujeto.rol_en IS NULL OR rol_sujeto.rol_en <= EXCLUDED.rol_en THEN EXCLUDED.rol_en ELSE rol_sujeto.rol_en END,
		 idp_subject = COALESCE(EXCLUDED.idp_subject, rol_sujeto.idp_subject)`
	// Revocar el rol X solo quita el rol vigente si sigue siendo X (o si no hay rol); en
	// ambos casos, si el hecho es el más nuevo, rol_en queda como lápida contra un
	// rol.asignado más viejo que llegue después.
	sqlRevocado = `INSERT INTO mensajeria.rol_sujeto (sujeto_id, rol, rol_en, idp_subject) VALUES ($1, NULL, $3, NULLIF($4, ''))
		ON CONFLICT (sujeto_id) DO UPDATE SET
		 rol = CASE WHEN (rol_sujeto.rol IS NULL OR rol_sujeto.rol = $2) AND (rol_sujeto.rol_en IS NULL OR rol_sujeto.rol_en <= EXCLUDED.rol_en) THEN NULL ELSE rol_sujeto.rol END,
		 rol_en = CASE WHEN (rol_sujeto.rol IS NULL OR rol_sujeto.rol = $2) AND (rol_sujeto.rol_en IS NULL OR rol_sujeto.rol_en <= EXCLUDED.rol_en) THEN EXCLUDED.rol_en ELSE rol_sujeto.rol_en END,
		 idp_subject = COALESCE(EXCLUDED.idp_subject, rol_sujeto.idp_subject)`
	sqlActivo = `INSERT INTO mensajeria.rol_sujeto (sujeto_id, activo, activo_en, idp_subject) VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (sujeto_id) DO UPDATE SET
		 activo = CASE WHEN rol_sujeto.activo_en IS NULL OR rol_sujeto.activo_en <= EXCLUDED.activo_en THEN EXCLUDED.activo ELSE rol_sujeto.activo END,
		 activo_en = CASE WHEN rol_sujeto.activo_en IS NULL OR rol_sujeto.activo_en <= EXCLUDED.activo_en THEN EXCLUDED.activo_en ELSE rol_sujeto.activo_en END,
		 idp_subject = COALESCE(EXCLUDED.idp_subject, rol_sujeto.idp_subject)`
)

// Procesar aplica el evento. Otro tipo, un payload ilegible o sin sujeto_id o
// instante se acusa sin efecto (reintentarlo daría lo mismo); un error de base
// se devuelve para reintentar.
func (c *ConsumidorRoles) Procesar(ctx context.Context, s bus.Sobre) error {
	var p payloadRol
	if json.Unmarshal(s.Payload, &p) != nil || p.SujetoID == "" {
		return nil
	}
	var err error
	switch s.Tipo {
	case TipoRolAsignado:
		if p.Rol == "" || p.AsignadoEn.IsZero() {
			return nil
		}
		_, err = c.db.Exec(ctx, sqlAsignado, p.SujetoID, p.Rol, p.AsignadoEn.UTC(), p.IdpSubject)
	case TipoRolRevocado:
		if p.Rol == "" || p.RevocadoEn.IsZero() {
			return nil
		}
		_, err = c.db.Exec(ctx, sqlRevocado, p.SujetoID, p.Rol, p.RevocadoEn.UTC(), p.IdpSubject)
	case TipoSujetoDesactivado:
		if p.DesactivadoEn.IsZero() {
			return nil
		}
		_, err = c.db.Exec(ctx, sqlActivo, p.SujetoID, false, p.DesactivadoEn.UTC(), p.IdpSubject)
	case TipoSujetoReactivado:
		if p.ReactivadoEn.IsZero() {
			return nil
		}
		_, err = c.db.Exec(ctx, sqlActivo, p.SujetoID, true, p.ReactivadoEn.UTC(), p.IdpSubject)
	default:
		return nil
	}
	if err != nil {
		return fmt.Errorf("pool: proyectar %s de %s: %w", s.Tipo, p.SujetoID, err)
	}
	return nil
}
