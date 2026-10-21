// Package conversacion contiene los handlers HTTP ENTRANTES de la conversación
// de negocio (arista bff → mensajeria, siempre HTTP; addendum §5). Hoy monta
// ReclamarConversacion sobre internal/reclamos detrás de la guarda de
// internal/autorizacion (DEC-097). EnviarMensaje NO se monta aún: su respuesta
// de contrato exige alias_mostrado (identidad comercial por cliente, DEC-097
// forma 1), que se persiste en el camino de escritura del dominio; ese cambio va
// en su propio slice.
//
// Los tipos de request/response son los generados de pf-pkg-contracts (tracer.7),
// nunca escritos a mano (golang-best-practices §3, regla 6).
package conversacion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/reclamos"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
)

// Autorizador decide el acceso a una conversación (internal/autorizacion).
type Autorizador interface {
	PuedeEscribir(ctx context.Context, tenantID, conversacionID string, c *auth.Claims) error
}

// Reclamador toma el reclamo activo de una conversación (internal/reclamos).
type Reclamador interface {
	Reclamar(ctx context.Context, tenantID, conversacionID, actorReal string, esperada msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error)
}

// LectorReclamo hidrata el Reclamo del contrato desde la fila recién escrita. La
// hidratación vive en la frontera: el dominio devuelve solo la versión nueva, y
// el contrato entrega el agregado completo (addendum §4). No toca el camino
// caliente de internal/reclamos.
type LectorReclamo interface {
	ReclamoActivo(ctx context.Context, tenantID, conversacionID string) (*msgclient.Reclamo, error)
}

// Handlers agrupa las dependencias de los handlers de conversación.
type Handlers struct {
	Autz     Autorizador
	Reclamos Reclamador
	Reclamo  LectorReclamo
}

// Montar registra las rutas de conversación en el mux. El bff antepone
// auth.Middleware, de modo que aquí los claims ya están en el contexto.
func (h Handlers) Montar(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/conversaciones/{id}/reclamos", h.reclamar)
}

// reclamar toma el reclamo activo de una conversación. Exige claims, autoriza
// contra la membresía del pool (DEC-097), reclama con la versión esperada y
// devuelve 201 con el Reclamo. El conflicto de versión del dominio se traduce a
// 409 (addendum §4 punto 5).
func (h Handlers) reclamar(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	convID := r.PathValue("id")

	if err := h.Autz.PuedeEscribir(r.Context(), claims.TenantID, convID, claims); err != nil {
		escribirError(w, err)
		return
	}

	var entrada msgclient.VersionEsperadaEntrada
	if err := json.NewDecoder(r.Body).Decode(&entrada); err != nil {
		escribirError(w, platformerrors.NewValidacion("cuerpo inválido"))
		return
	}

	_, err := h.Reclamos.Reclamar(r.Context(), claims.TenantID, convID, claims.Subject, msgtipos.VersionReclamo(entrada.VersionEsperada))
	if err != nil {
		var conflicto reclamos.ErrConflictoVersion
		if errors.As(err, &conflicto) {
			escribirError(w, platformerrors.NewConflicto("conflicto de versión del reclamo"))
			return
		}
		escribirError(w, traducir(err))
		return
	}

	reclamo, err := h.Reclamo.ReclamoActivo(r.Context(), claims.TenantID, convID)
	if err != nil {
		// shortcut: hidratación en una lectura aparte del reclamo, no en la tx del
		// Reclamar; una liberación concurrente entre ambas podría dar ErrSinReclamo.
		// Upgrade cuando Reclamar devuelva el agregado completo en su propia tx.
		escribirError(w, traducir(err))
		return
	}
	escribirJSON(w, http.StatusCreated, reclamo)
}

// traducir conserva la categoría de un PlatformError y envuelve cualquier otro
// error como roto (502): un error de dominio sin categoría es un fallo del
// servidor, no del llamador.
func traducir(err error) error {
	if platformerrors.AsCategory(err) != platformerrors.CategoryUnknown {
		return err
	}
	return platformerrors.NewRotoErr(err, "mensajeria: error interno")
}

// escribirJSON serializa v como JSON con el status dado.
func escribirJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// escribirError traduce un error de la taxonomía de la plataforma a su status
// HTTP (invariante I3) y lo emite con la forma Error del contrato.
func escribirError(w http.ResponseWriter, err error) {
	cat := platformerrors.AsCategory(err)
	escribirJSON(w, cat.HTTPStatus(), msgclient.Error{
		Codigo:  cat.String(),
		Mensaje: err.Error(),
	})
}
