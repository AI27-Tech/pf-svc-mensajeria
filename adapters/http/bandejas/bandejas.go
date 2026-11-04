// Package bandejas contiene el handler HTTP entrante del trabajo de una bandeja
// (arista bff → mensajeria). Monta listarTrabajoBandeja (contrato tracer.14, M3):
// el bootstrap y el catch-up incremental de la colección de bandeja (ADR 28 §3).
// Detrás de la guarda de internal/autorizacion: membresía por rol, sin acotar
// por el tenant de quien pregunta (DEC-127, DEC-129, DEC-130, DEC-131).
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
	Bandeja(ctx context.Context, bandejaID string, c *auth.Claims) (string, error)
}

// Trabajador sirve el bootstrap y el catch-up de una bandeja (internal/bandejas).
type Trabajador interface {
	// lector es autorizacion.LectorDirigidas: el Ejecutivo no ve las
	// conversaciones dirigidas a otro (DEC-128).
	Trabajo(ctx context.Context, tenantID, bandejaID, lector string) ([]msgclient.Conversacion, int64, error)
	CambiosDesde(ctx context.Context, tenantID, bandejaID, lector string, desde int64) ([]msgclient.Conversacion, int64, bool, error)
	// TrabajoChat y CambiosChatDesde sirven la unión de las bandejas de chat de
	// todos los tenants, la bandeja del sujeto interno (DEC-129).
	TrabajoChat(ctx context.Context, lector string) ([]msgclient.Conversacion, int64, error)
	CambiosChatDesde(ctx context.Context, lector string, desde int64) ([]msgclient.Conversacion, int64, bool, error)
}

// SinBandejaChat es el mensaje del 404 de la bandeja del sujeto: rol que no la
// ve. Lo comparte la superficie en proceso.
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
// Autoriza con autorizacion.Bandeja, que devuelve el tenant de la bandeja: las
// lecturas usan ese tenant, no el del token (DEC-129).
func (h Handlers) trabajo(w http.ResponseWriter, r *http.Request) {
	claims := boundary.Claims(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	bandejaID := r.PathValue("id")

	tenant, err := h.Autz.Bandeja(r.Context(), bandejaID, claims)
	if err != nil {
		escribirError(w, err)
		return
	}
	lector := autorizacion.LectorDirigidas(claims)
	h.servirTrabajo(w, r,
		func() ([]msgclient.Conversacion, int64, error) {
			return h.Bandeja.Trabajo(r.Context(), tenant, bandejaID, lector)
		},
		func(desde int64) ([]msgclient.Conversacion, int64, bool, error) {
			return h.Bandeja.CambiosDesde(r.Context(), tenant, bandejaID, lector, desde)
		})
}

// miTrabajo sirve el trabajo de la bandeja del SUJETO, sin {id}: la unión de las
// bandejas de chat de todos los tenants (DEC-127, DEC-129). La ven el Ejecutivo y,
// en lectura, Supervisor y Coordinador (DEC-131); cualquier otro rol y el cliente
// reciben 404 (no tienen bandeja, contrato listarTrabajoMiBandeja). El Ejecutivo
// no ve las conversaciones dirigidas a otro Ejecutivo (DEC-128).
func (h Handlers) miTrabajo(w http.ResponseWriter, r *http.Request) {
	claims := boundary.Claims(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	if claims.Audiencia == auth.AudienciaCliente || !autorizacion.LeeBandejaChat(claims.Rol) {
		escribirError(w, platformerrors.NewNoEncontrado(SinBandejaChat))
		return
	}
	lector := autorizacion.LectorDirigidas(claims)
	h.servirTrabajo(w, r,
		func() ([]msgclient.Conversacion, int64, error) { return h.Bandeja.TrabajoChat(r.Context(), lector) },
		func(desde int64) ([]msgclient.Conversacion, int64, bool, error) {
			return h.Bandeja.CambiosChatDesde(r.Context(), lector, desde)
		})
}

// servirTrabajo es el cuerpo común de las dos rutas de trabajo: lee
// desde_revision (bootstrap sin él, catch-up con él) y escribe TrabajoBandeja con
// la revisión de la colección. La colección ya viene resuelta y autorizada.
func (h Handlers) servirTrabajo(w http.ResponseWriter, r *http.Request,
	bootstrap func() ([]msgclient.Conversacion, int64, error),
	catchUp func(desde int64) ([]msgclient.Conversacion, int64, bool, error),
) {
	crudo := r.URL.Query().Get("desde_revision")
	var (
		items []msgclient.Conversacion
		rev   int64
		fuera bool
		err   error
	)
	if crudo == "" {
		items, rev, err = bootstrap()
	} else {
		var desde int64
		desde, err = strconv.ParseInt(crudo, 10, 64)
		if err != nil {
			escribirError(w, platformerrors.NewValidacion("desde_revision inválida"))
			return
		}
		items, rev, fuera, err = catchUp(desde)
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
		escribirError(w, platformerrors.NewConflictoCodigo("stream_resync_required", "mensajeria: stream_resync_required"))
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
