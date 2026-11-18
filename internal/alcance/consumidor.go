package alcance

import (
	"context"
	"encoding/json"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/google/uuid"
)

// TipoAlcanceCambiado es el evento que alimenta la proyección (registro.tipos.yaml,
// tracer.41): productor identidad, consumidor durable mensajeria.
const TipoAlcanceCambiado = "alcance.cambiado"

// payload es AlcanceCambiado de sujeto.proto tal como identidad lo publica en
// JSON (sujeto_id, idp_subject, clientes_efectivos_ids, cambiado_en en RFC
// 3339). La proyección se indexa por idp_subject, el `sub` del token (DEC-149
// #22, tracer.42); sujeto_id no se usa.
type payload struct {
	IdpSubject           string    `json:"idp_subject"`
	ClientesEfectivosIDs []string  `json:"clientes_efectivos_ids"`
	CambiadoEn           time.Time `json:"cambiado_en"`
}

// Consumidor aplica cada alcance.cambiado a la proyección. No deduplica por
// evento_id: el efecto ya es idempotente (snapshot completo con orden por
// cambiado_en), así que una reentrega no cambia nada.
type Consumidor struct {
	proy *Proyeccion
}

// NuevoConsumidor crea el consumidor sobre la proyección.
func NuevoConsumidor(proy *Proyeccion) *Consumidor { return &Consumidor{proy: proy} }

// Procesar maneja un sobre del bus. Otro tipo se acusa sin efecto. Un payload
// ilegible (JSON inválido, sin idp_subject o sin cambiado_en, ids que no son
// UUID) se acusa sin aplicar, como en los demás consumidores: reintentarlo daría
// lo mismo y bloquearía la cola. Un error de base se devuelve para reintentar.
func (c *Consumidor) Procesar(ctx context.Context, s bus.Sobre) error {
	if s.Tipo != TipoAlcanceCambiado {
		return nil
	}
	a, ok := leer(s.Payload)
	if !ok {
		return nil
	}
	return c.proy.Aplicar(ctx, a)
}

func leer(raw []byte) (Alcance, bool) {
	var p payload
	if json.Unmarshal(raw, &p) != nil || p.IdpSubject == "" || p.CambiadoEn.IsZero() {
		return Alcance{}, false
	}
	a := Alcance{IdpSubject: p.IdpSubject, CambiadoEn: p.CambiadoEn, Clientes: make([]uuid.UUID, 0, len(p.ClientesEfectivosIDs))}
	for _, id := range p.ClientesEfectivosIDs {
		u, err := uuid.Parse(id)
		if err != nil {
			return Alcance{}, false
		}
		a.Clientes = append(a.Clientes, u)
	}
	return a, true
}
