// Package bandejas contiene el handler HTTP entrante del trabajo de una bandeja
// (arista bff → mensajeria). Monta listarTrabajoBandeja (contrato tracer.14, M3):
// el bootstrap y el catch-up incremental de la colección de bandeja (ADR 28 §3).
// Detrás de la guarda de internal/autorizacion (membresía de pool).
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
}

// Handlers agrupa las dependencias del handler de bandejas.
type Handlers struct {
	Autz    Autorizador
	Bandeja Trabajador
}

// Montar registra la ruta de trabajo de bandeja. El bff antepone auth.Middleware.
func (h Handlers) Montar(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/bandejas/{id}/trabajo", h.trabajo)
}

// trabajo sirve el trabajo de una bandeja. Sin desde_revision es el bootstrap;
// con desde_revision es el catch-up incremental. Autoriza por membresía de pool
// (DEC-097, Addendum §7) y devuelve TrabajoBandeja con la revisión de la
// colección. Fuera de ventana (no en V1) sería 409 stream_resync_required.
func (h Handlers) trabajo(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	bandejaID := r.PathValue("id")

	if err := h.Autz.PuedeVerBandeja(r.Context(), claims.TenantID, bandejaID, claims); err != nil {
		escribirError(w, err)
		return
	}

	crudo := r.URL.Query().Get("desde_revision")
	var (
		items []msgclient.Conversacion
		rev   int64
		fuera bool
		err   error
	)
	if crudo == "" {
		items, rev, err = h.Bandeja.Trabajo(r.Context(), claims.TenantID, bandejaID)
	} else {
		var desde int64
		desde, err = strconv.ParseInt(crudo, 10, 64)
		if err != nil {
			escribirError(w, platformerrors.NewValidacion("desde_revision inválida"))
			return
		}
		items, rev, fuera, err = h.Bandeja.CambiosDesde(r.Context(), claims.TenantID, bandejaID, desde)
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

func escribirError(w http.ResponseWriter, err error) {
	cat := platformerrors.AsCategory(err)
	escribirJSON(w, cat.HTTPStatus(), msgclient.Error{
		Codigo:  cat.String(),
		Mensaje: err.Error(),
	})
}
