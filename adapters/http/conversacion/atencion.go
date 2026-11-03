package conversacion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/auditoria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
)

// Atendedor marca atendida y registra recibos (internal/conversacion, E3-T6).
type Atendedor interface {
	MarcarAtendida(ctx context.Context, tenantID, conversacionID, actorReal string, esperada msgtipos.VersionReclamo) error
	RegistrarRecibo(ctx context.Context, tenantID, conversacionID, sujetoID string, tipo conversacion.Recibo, hasta msgtipos.SecuenciaMensaje) error
}

// LectorAuditoria lee la auditoría inmutable de una conversación (internal/auditoria).
type LectorAuditoria interface {
	Listar(ctx context.Context, tenantID, conversacionID, cursor string) (*msgclient.PaginaAuditoria, error)
}

// RolAdministrador es el único rol que lee la auditoría (contrato
// listarAuditoriaConversacion, DEC-036; el rol Auditor no existe en V1).
const RolAdministrador = "administrador"

// TraducirAtencion mapea los errores de dominio de atendida, recibos y auditoría
// a la taxonomía de la plataforma. Exportada para que la superficie en proceso
// (mensajeria.ConversacionesAPI) traduzca IGUAL que estos handlers.
//
// El contrato declara 403 para "no es el responsable"; la taxonomía de la
// plataforma no tiene categoría prohibido, así que sale como no_autorizado
// (401), igual que las demás denegaciones de este servicio.
func TraducirAtencion(err error) error {
	switch {
	case errors.Is(err, conversacion.ErrNoEsResponsable):
		return platformerrors.NewNoAutorizado("solo el responsable activo marca atendida")
	case errors.Is(err, conversacion.ErrSinReclamoActivo):
		return platformerrors.NewConflicto("conflicto_version_reclamo")
	case errors.Is(err, conversacion.ErrConversacionNoEncontrada), errors.Is(err, auditoria.ErrConversacionNoEncontrada):
		return platformerrors.NewNoEncontrado("conversacion no encontrada")
	case errors.Is(err, auditoria.ErrCursorInvalido):
		return platformerrors.NewValidacion("cursor inválido")
	}
	return traducir(err)
}

// marcarAtendida atiende POST /api/v1/conversaciones/{id}/atendida: acción
// explícita del responsable con versión esperada (DEC-117). 204 si se marcó.
func (h Handlers) marcarAtendida(w http.ResponseWriter, r *http.Request) {
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

	if err := h.Atencion.MarcarAtendida(r.Context(), claims.TenantID, convID, claims.Subject, msgtipos.VersionReclamo(entrada.VersionEsperada)); err != nil {
		escribirError(w, TraducirAtencion(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// registrarRecibo devuelve el handler de POST .../recibos/entregado|leido: avanza
// el cursor del llamante (cliente dueño o miembro del pool). 204 aunque el
// cursor ya estuviera más adelante (contrato).
func (h Handlers) registrarRecibo(tipo conversacion.Recibo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		var entrada msgclient.ReciboEntrada
		if err := json.NewDecoder(r.Body).Decode(&entrada); err != nil {
			escribirError(w, platformerrors.NewValidacion("cuerpo inválido"))
			return
		}

		if err := h.Atencion.RegistrarRecibo(r.Context(), claims.TenantID, convID, claims.Subject, tipo, msgtipos.SecuenciaMensaje(entrada.HastaSecuencia)); err != nil {
			escribirError(w, TraducirAtencion(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// listarAuditoria atiende GET /api/v1/auditoria/conversaciones/{id}: solo
// administrador (DEC-036). El rol se verifica antes de leer, así que un no
// administrador no distingue una conversación existente de una inexistente.
func (h Handlers) listarAuditoria(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	if claims.Rol != RolAdministrador {
		escribirError(w, platformerrors.NewNoAutorizado("solo administrador lee la auditoría"))
		return
	}

	pagina, err := h.Auditoria.Listar(r.Context(), claims.TenantID, r.PathValue("id"), r.URL.Query().Get("cursor"))
	if err != nil {
		escribirError(w, TraducirAtencion(err))
		return
	}
	escribirJSON(w, http.StatusOK, pagina)
}
