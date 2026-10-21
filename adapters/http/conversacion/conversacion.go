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
	"strconv"

	catclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/reclamos"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
	"github.com/google/uuid"
)

// Autorizador decide el acceso a una conversación (internal/autorizacion).
type Autorizador interface {
	PuedeEscribir(ctx context.Context, tenantID, conversacionID string, c *auth.Claims) error
}

// Reclamador toma, libera y transfiere el reclamo de una conversación
// (internal/reclamos).
type Reclamador interface {
	Reclamar(ctx context.Context, tenantID, conversacionID, actorReal string, esperada msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error)
	Liberar(ctx context.Context, tenantID, conversacionID, reclamoID, actorReal string, esperada msgtipos.VersionReclamo) error
	Transferir(ctx context.Context, tenantID, conversacionID, origen, destino, motivo string, esperada msgtipos.VersionReclamo) error
}

// LectorReclamo hidrata el Reclamo del contrato desde la fila recién escrita. La
// hidratación vive en la frontera: el dominio devuelve solo la versión nueva, y
// el contrato entrega el agregado completo (addendum §4). No toca el camino
// caliente de internal/reclamos.
type LectorReclamo interface {
	ReclamoActivo(ctx context.Context, tenantID, conversacionID string) (*msgclient.Reclamo, error)
}

// Enviador envía un mensaje, hidrata el Mensaje del contrato y resuelve el
// cliente de la conversación para el alias (internal/conversacion).
type Enviador interface {
	Enviar(ctx context.Context, in conversacion.EntradaEnvio) (conversacion.ResultadoEnvio, error)
	MensajePorID(ctx context.Context, tenantID, mensajeID string) (*msgclient.Mensaje, error)
	ClienteDeConversacion(ctx context.Context, tenantID, conversacionID string) (string, error)
}

// CatalogosLector lee clientes por lote (puerto de mensajeria hacia catalogos).
// El handler lo usa para resolver el alias comercial por cliente (Opción 2,
// DEC-097). Puede ser nil: sin él el alias queda vacío y el envío sigue.
type CatalogosLector interface {
	ListarClientes(ctx context.Context, ids []catclient.Id) (*catclient.PaginaClientes, error)
}

// Handlers agrupa las dependencias de los handlers de conversación.
type Handlers struct {
	Autz         Autorizador
	Reclamos     Reclamador
	Reclamo      LectorReclamo
	Conversacion Enviador
	Catalogos    CatalogosLector
}

// Montar registra las rutas de conversación en el mux. El bff antepone
// auth.Middleware, de modo que aquí los claims ya están en el contexto.
func (h Handlers) Montar(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/conversaciones/{id}/reclamos", h.reclamar)
	mux.HandleFunc("DELETE /api/v1/conversaciones/{id}/reclamos/{rid}", h.liberar)
	mux.HandleFunc("POST /api/v1/conversaciones/{id}/transferencias", h.transferir)
	mux.HandleFunc("POST /api/v1/conversaciones/{id}/mensajes", h.enviar)
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

// liberar cierra el reclamo activo de una conversación y lo devuelve al pool
// (addendum §4 punto 6). DELETE sobre el reclamo por su rid, con version_esperada
// obligatoria en el query (concurrencia optimista). Autoriza (DEC-097), traduce
// el conflicto de versión a 409 y la ausencia de reclamo a 404, y responde 204.
func (h Handlers) liberar(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	convID := r.PathValue("id")
	rid := r.PathValue("rid")

	ver, err := strconv.ParseInt(r.URL.Query().Get("version_esperada"), 10, 64)
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("version_esperada inválida o faltante"))
		return
	}

	if err := h.Autz.PuedeEscribir(r.Context(), claims.TenantID, convID, claims); err != nil {
		escribirError(w, err)
		return
	}

	err = h.Reclamos.Liberar(r.Context(), claims.TenantID, convID, rid, claims.Subject, msgtipos.VersionReclamo(ver))
	if err != nil {
		var conflicto reclamos.ErrConflictoVersion
		if errors.As(err, &conflicto) {
			escribirError(w, platformerrors.NewConflicto("conflicto de versión del reclamo"))
			return
		}
		if errors.Is(err, reclamos.ErrSinReclamo) {
			escribirError(w, platformerrors.NewNoEncontrado("reclamo no encontrado"))
			return
		}
		escribirError(w, traducir(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// transferir mueve la responsabilidad de la conversación a otro miembro del pool
// (addendum §4 punto 6). POST con TransferenciaEntrada (destino, versión
// esperada, motivo opcional). Autoriza (DEC-097) al que transfiere, cierra y abre
// el reclamo en una tx del dominio, y devuelve 201 con el nuevo Reclamo. Conflicto
// de versión → 409; sin reclamo activo → 404.
func (h Handlers) transferir(w http.ResponseWriter, r *http.Request) {
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

	var entrada msgclient.TransferenciaEntrada
	if err := json.NewDecoder(r.Body).Decode(&entrada); err != nil {
		escribirError(w, platformerrors.NewValidacion("cuerpo inválido"))
		return
	}
	motivo := ""
	if entrada.Motivo != nil {
		motivo = *entrada.Motivo
	}

	err := h.Reclamos.Transferir(r.Context(), claims.TenantID, convID, claims.Subject,
		entrada.ResponsableDestinoId.String(), motivo, msgtipos.VersionReclamo(entrada.VersionEsperada))
	if err != nil {
		var conflicto reclamos.ErrConflictoVersion
		if errors.As(err, &conflicto) {
			escribirError(w, platformerrors.NewConflicto("conflicto de versión del reclamo"))
			return
		}
		if errors.Is(err, reclamos.ErrSinReclamo) {
			escribirError(w, platformerrors.NewNoEncontrado("reclamo no encontrado"))
			return
		}
		escribirError(w, traducir(err))
		return
	}

	reclamo, err := h.Reclamo.ReclamoActivo(r.Context(), claims.TenantID, convID)
	if err != nil {
		escribirError(w, traducir(err))
		return
	}
	escribirJSON(w, http.StatusCreated, reclamo)
}

// enviar escribe un mensaje en la conversación. Exige claims, autoriza (DEC-097),
// resuelve el alias comercial desde el cliente de la conversación (Opción 2) y
// delega en el dominio, que persiste mensaje + outbox en una tx. Devuelve 201
// con el Mensaje hidratado (alias y audiencia incluidos). Idempotente por
// client_message_id: un reintento devuelve el mensaje original.
func (h Handlers) enviar(w http.ResponseWriter, r *http.Request) {
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

	var entrada msgclient.EnviarMensajeEntrada
	if err := json.NewDecoder(r.Body).Decode(&entrada); err != nil {
		escribirError(w, platformerrors.NewValidacion("cuerpo inválido"))
		return
	}

	alias, err := h.resolverAlias(r.Context(), claims.TenantID, convID)
	if err != nil {
		escribirError(w, traducir(err))
		return
	}

	res, err := h.Conversacion.Enviar(r.Context(), conversacion.EntradaEnvio{
		TenantID:        claims.TenantID,
		ConversacionID:  convID,
		ActorReal:       claims.Subject,
		ClientMessageID: entrada.ClientMessageId.String(),
		Cuerpo:          entrada.Cuerpo,
		AliasMostrado:   alias,
		RolAlEnviar:     claims.Rol,
	})
	if err != nil {
		escribirError(w, traducir(err))
		return
	}

	mensaje, err := h.Conversacion.MensajePorID(r.Context(), claims.TenantID, res.MensajeID)
	if err != nil {
		escribirError(w, traducir(err))
		return
	}
	escribirJSON(w, http.StatusCreated, mensaje)
}

// resolverAlias obtiene el nombre comercial que ve el cliente (Opción 2): el
// nombre del cliente de la conversación, leído por lote desde catalogos. Sin
// puerto de catalogos o sin cliente en la conversación, devuelve vacío: el alias
// queda NULL y el envío sigue (la degradación de catalogos no rompe el envío,
// mensajeria.md (f)).
func (h Handlers) resolverAlias(ctx context.Context, tenantID, conversacionID string) (string, error) {
	if h.Catalogos == nil {
		return "", nil
	}
	clienteID, err := h.Conversacion.ClienteDeConversacion(ctx, tenantID, conversacionID)
	if err != nil {
		return "", err
	}
	if clienteID == "" {
		return "", nil
	}
	id, err := uuid.Parse(clienteID)
	if err != nil {
		return "", nil
	}
	pagina, err := h.Catalogos.ListarClientes(ctx, []catclient.Id{id})
	if err != nil || pagina == nil || len(pagina.Items) == 0 {
		return "", nil // degradado: sin alias, no se rompe el envío
	}
	return pagina.Items[0].Nombre, nil
}

// traducir conserva la categoría de un PlatformError y envuelve cualquier otro
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
