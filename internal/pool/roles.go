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

const (
	sqlAsignado = `INSERT INTO mensajeria.rol_sujeto (sujeto_id, rol, rol_en) VALUES ($1, $2, $3)
		ON CONFLICT (sujeto_id) DO UPDATE SET rol = EXCLUDED.rol, rol_en = EXCLUDED.rol_en
		 WHERE rol_sujeto.rol_en IS NULL OR rol_sujeto.rol_en <= EXCLUDED.rol_en`
	// Revocar el rol X solo quita el rol vigente si sigue siendo X.
	sqlRevocado = `INSERT INTO mensajeria.rol_sujeto (sujeto_id, rol, rol_en) VALUES ($1, NULL, $3)
		ON CONFLICT (sujeto_id) DO UPDATE SET rol = NULL, rol_en = EXCLUDED.rol_en
		 WHERE rol_sujeto.rol = $2 AND (rol_sujeto.rol_en IS NULL OR rol_sujeto.rol_en <= EXCLUDED.rol_en)`
	sqlActivo = `INSERT INTO mensajeria.rol_sujeto (sujeto_id, activo, activo_en) VALUES ($1, $2, $3)
		ON CONFLICT (sujeto_id) DO UPDATE SET activo = EXCLUDED.activo, activo_en = EXCLUDED.activo_en
		 WHERE rol_sujeto.activo_en IS NULL OR rol_sujeto.activo_en <= EXCLUDED.activo_en`
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
		_, err = c.db.Exec(ctx, sqlAsignado, p.SujetoID, p.Rol, p.AsignadoEn.UTC())
	case TipoRolRevocado:
		if p.Rol == "" || p.RevocadoEn.IsZero() {
			return nil
		}
		_, err = c.db.Exec(ctx, sqlRevocado, p.SujetoID, p.Rol, p.RevocadoEn.UTC())
	case TipoSujetoDesactivado:
		if p.DesactivadoEn.IsZero() {
			return nil
		}
		_, err = c.db.Exec(ctx, sqlActivo, p.SujetoID, false, p.DesactivadoEn.UTC())
	case TipoSujetoReactivado:
		if p.ReactivadoEn.IsZero() {
			return nil
		}
		_, err = c.db.Exec(ctx, sqlActivo, p.SujetoID, true, p.ReactivadoEn.UTC())
	default:
		return nil
	}
	if err != nil {
		return fmt.Errorf("pool: proyectar %s de %s: %w", s.Tipo, p.SujetoID, err)
	}
	return nil
}
