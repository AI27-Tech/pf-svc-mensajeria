// Package bandejas contiene el handler HTTP entrante del trabajo de una bandeja
// (arista bff → mensajeria). Monta listarTrabajoBandeja (contrato tracer.14, M3):
// el bootstrap y el catch-up incremental de la colección de bandeja (ADR 28 §3).
// Detrás de la guarda de internal/autorizacion (rol para la bandeja de chat,
// membresía de pool para Reacción; DEC-127).
package bandejas

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-mensajeria/adapters/http/boundary"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/autorizacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/bandejas"
)

// Autorizador decide el acceso a una bandeja (internal/autorizacion).
type Autorizador interface {
	PuedeVerBandeja(ctx context.Context, tenantID, bandejaID string, c *auth.Claims) error
}

// Trabajador sirve el bootstrap y el catch-up de una bandeja (internal/bandejas).
type Trabajador interface {
	Trabajo(ctx context.Context, tenantID, bandejaID string) ([]msgclient.Conversacion, int64, error)
	CambiosDesde(ctx context.Context, tenantID, bandejaID string, desde int64) ([]msgclient.Conversacion, int64, bool, error)
	// ResolverMiBandeja da el id de la bandeja única de conversaciones del tenant
	// (DEC-127). Sin bandeja de chat -> ErrBandejaNoEncontrada.
	ResolverMiBandeja(ctx context.Context, tenantID string) (string, error)
}

// SinBandejaChat es el mensaje del 404 de la bandeja del sujeto: rol que no es
// miembro o tenant sin bandeja de conversaciones. Lo comparte la superficie en
// proceso.
const SinBandejaChat = "el sujeto no pertenece a ningún pool de chat"

// Handlers agrupa las dependencias del handler de bandejas.
type Handlers struct {
	Autz    Autorizador
	Bandeja Trabajador
}

// Montar registra las rutas de trabajo de bandeja. El bff antepone auth.Middleware.
func (h Handlers) Montar(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/bandejas/{id}/trabajo", h.trabajo)
	mux.HandleFunc("GET /api/v1/bandeja/trabajo", h.miTrabajo)
}

// trabajo sirve el trabajo de una bandeja NOMBRADA por {id} (uso administrativo).
// Autoriza con PuedeVerBandeja: rol `ejecutivo` en la de chat (DEC-127),
// membresía de pool en la de Reacción (Addendum §7).
func (h Handlers) trabajo(w http.ResponseWriter, r *http.Request) {
	claims := boundary.Claims(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	bandejaID := r.PathValue("id")

	if err := h.Autz.PuedeVerBandeja(r.Context(), claims.TenantID, bandejaID, claims); err != nil {
		escribirError(w, err)
		return
	}
	h.servirTrabajo(w, r, claims.TenantID, bandejaID)
}

// miTrabajo sirve el trabajo de la bandeja del SUJETO, sin {id} (bandeja única,
// DEC-127). Todo Ejecutivo es miembro por su rol; cualquier otro rol (y el
// cliente) recibe 404, la misma respuesta que antes recibía quien no tenía
// membresía. Ser Ejecutivo y resolver la bandeja del tenant ES la autorización,
// así que no hace falta una guarda PuedeVerBandeja aparte.
func (h Handlers) miTrabajo(w http.ResponseWriter, r *http.Request) {
	claims := boundary.Claims(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	if !autorizacion.EsMiembroBandejaChat(claims.Rol) {
		escribirError(w, platformerrors.NewNoEncontrado(SinBandejaChat))
		return
	}
	bandejaID, err := h.Bandeja.ResolverMiBandeja(r.Context(), claims.TenantID)
	if err != nil {
		if errors.Is(err, bandejas.ErrBandejaNoEncontrada) {
			escribirError(w, platformerrors.NewNoEncontrado(SinBandejaChat))
			return
		}
		escribirError(w, traducir(err))
		return
	}
	h.servirTrabajo(w, r, claims.TenantID, bandejaID)
}

// servirTrabajo es el cuerpo común de las dos rutas de trabajo: lee
// desde_revision (bootstrap sin él, catch-up con él) y escribe TrabajoBandeja con
// la revisión de la colección. El id de bandeja ya viene resuelto y autorizado.
func (h Handlers) servirTrabajo(w http.ResponseWriter, r *http.Request, tenantID, bandejaID string) {
	crudo := r.URL.Query().Get("desde_revision")
	var (
		items []msgclient.Conversacion
		rev   int64
		fuera bool
		err   error
	)
	if crudo == "" {
		items, rev, err = h.Bandeja.Trabajo(r.Context(), tenantID, bandejaID)
	} else {
		var desde int64
		desde, err = strconv.ParseInt(crudo, 10, 64)
		if err != nil {
			escribirError(w, platformerrors.NewValidacion("desde_revision inválida"))
			return
		}
		items, rev, fuera, err = h.Bandeja.CambiosDesde(r.Context(), tenantID, bandejaID, desde)
	}
	if err != nil {
		if errors.Is(err, bandejas.ErrBandejaNoEncontrada) {
			escribirError(w, platformerrors.NewNoEncontrado("bandeja no encontrada"))
			return
		}
		escribirError(w, traducir(err))
		return
	}
	if fuera {
		escribirError(w, platformerrors.NewConflicto("stream_resync_required"))
		return
	}

	if items == nil {
		items = []msgclient.Conversacion{}
	}
	escribirJSON(w, http.StatusOK, msgclient.TrabajoBandeja{
		Items:    items,
		Revision: rev,
	})
}

func traducir(err error) error {
	if platformerrors.AsCategory(err) != platformerrors.CategoryUnknown {
		return err
	}
	return platformerrors.NewRotoErr(err, "mensajeria: error interno")
}

func escribirJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// escribirError emite el error sin su causa interna (boundary.WriteError).
func escribirError(w http.ResponseWriter, err error) { boundary.WriteError(w, err) }
