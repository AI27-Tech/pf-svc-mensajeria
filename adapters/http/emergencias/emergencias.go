// Package emergencias es el adaptador HTTP entrante de la familia de emergencia
// (contrato tracer.17: /api/v1/viajes/{id}/emergencias y sus subrecursos). El
// bff antepone auth.Middleware, así que los claims llegan en el contexto. El
// dominio vive en internal/emergencias; esta capa decodifica el contrato,
// autoriza y traduce los errores de dominio a códigos HTTP.
package emergencias

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"

	"github.com/AI27-Tech/pf-svc-mensajeria/internal/emergencias"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
)

// Autorizador decide el acceso a un caso de emergencia (internal/autorizacion).
type Autorizador interface {
	PuedeAtenderEmergencia(ctx context.Context, tenantID, emergenciaID string, c *auth.Claims) error
}

// Emergencias es el caso de uso del dominio (internal/emergencias).
type Emergencias interface {
	Activar(ctx context.Context, in emergencias.EntradaActivacion) (emergencias.ResultadoActivacion, error)
	AgregarNota(ctx context.Context, in emergencias.EntradaNota) (string, error)
	Reclamar(ctx context.Context, tenantID, emergenciaID, actorReal string, esperada msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error)
	Liberar(ctx context.Context, tenantID, emergenciaID, reclamoID, actorReal string, esperada msgtipos.VersionReclamo) error
	Transferir(ctx context.Context, tenantID, emergenciaID, origen, destino string, esperada msgtipos.VersionReclamo) error
	Resolver(ctx context.Context, tenantID, emergenciaID, actorReal string, esperada msgtipos.VersionReclamo) error
	Cerrar(ctx context.Context, tenantID, emergenciaID, actorReal string) error
	ListarPorViaje(ctx context.Context, tenantID, viajeID string, soloActivos bool) ([]emergencias.CasoProyectado, error)
}

// Handlers agrupa las dependencias de los handlers de emergencia.
type Handlers struct {
	Autz        Autorizador
	Emergencias Emergencias
}

// Montar registra las rutas de emergencia en el mux.
func (h Handlers) Montar(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/viajes/{id}/emergencias", h.listar)
	mux.HandleFunc("POST /api/v1/viajes/{id}/emergencias", h.activar)
	mux.HandleFunc("POST /api/v1/viajes/{id}/emergencias/{eid}/notas", h.agregarNota)
	mux.HandleFunc("POST /api/v1/viajes/{id}/emergencias/{eid}/reclamos", h.reclamar)
	mux.HandleFunc("DELETE /api/v1/viajes/{id}/emergencias/{eid}/reclamos/{rid}", h.liberar)
	mux.HandleFunc("POST /api/v1/viajes/{id}/emergencias/{eid}/transferencias", h.transferir)
	mux.HandleFunc("POST /api/v1/viajes/{id}/emergencias/{eid}/resolucion", h.resolver)
	mux.HandleFunc("POST /api/v1/viajes/{id}/emergencias/{eid}/cierre", h.cerrar)
}

// listar devuelve los casos de emergencia del viaje, proyectados por audiencia
// (ADR 21 §6). Al Cliente le entrega la vista mínima: solo casos no cerrados y
// sin los campos internos (origen, actor, reclamo). A Interno, el caso completo.
func (h Handlers) listar(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	viajeID := r.PathValue("id")
	esCliente := claims.Audiencia == auth.AudienciaCliente

	casos, err := h.Emergencias.ListarPorViaje(r.Context(), claims.TenantID, viajeID, esCliente)
	if err != nil {
		escribirError(w, traducir(err))
		return
	}

	items := make([]msgclient.CasoEmergencia, 0, len(casos))
	for _, c := range casos {
		items = append(items, proyectar(c, esCliente))
	}
	escribirJSON(w, http.StatusOK, msgclient.PaginaEmergencias{Items: items})
}

// proyectar mapea un caso leído al CasoEmergencia del contrato. Para audiencia
// Cliente oculta los campos internos (origen, actor_real, reclamo_activo quedan
// en su cero), que solo se entregan a Interno (ADR 21 §3, §6).
func proyectar(c emergencias.CasoProyectado, esCliente bool) msgclient.CasoEmergencia {
	caso := msgclient.CasoEmergencia{
		Id:             uuidOCero(c.ID),
		ViajeId:        uuidOCero(c.ViajeID),
		Estado:         msgclient.EstadoEmergencia(c.Estado),
		VersionReclamo: int64(c.VersionReclamo),
		ActivadaEn:     c.ActivadaEn,
		ResueltaEn:     c.ResueltaEn,
		CerradaEn:      c.CerradaEn,
	}
	if c.ClienteID != "" {
		caso.ClienteId = uuidOCero(c.ClienteID)
	}
	if esCliente {
		return caso
	}
	// Interno ve los campos internos.
	caso.Origen = msgclient.OrigenActivacion(c.Origen)
	if id, err := uuid.Parse(c.ActorReal); err == nil {
		caso.ActorReal = &id
	}
	return caso
}

func uuidOCero(s string) msgclient.Id {
	id, _ := uuid.Parse(s)
	return id
}

// activar crea un caso de emergencia sobre el viaje. Cliente dentro de su
// organización o interno de monitoreo (ADR 21 §1; el alcance fino por viaje lo
// impone la composición de bff, igual que conversación no revalida el viaje).
// Idempotente por activacion_id: una reentrega devuelve 200 con el caso original.
func (h Handlers) activar(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	viajeID := r.PathValue("id")

	var entrada msgclient.ActivarEmergenciaEntrada
	if err := json.NewDecoder(r.Body).Decode(&entrada); err != nil {
		escribirError(w, platformerrors.NewValidacion("cuerpo inválido"))
		return
	}

	origen := msgclient.OrigenActivacionOperacion
	if claims.Audiencia == auth.AudienciaCliente {
		origen = msgclient.OrigenActivacionCliente
	}

	res, err := h.Emergencias.Activar(r.Context(), emergencias.EntradaActivacion{
		TenantID:     claims.TenantID,
		ViajeID:      viajeID,
		ClienteID:    clienteDeClaims(claims),
		ActorReal:    claims.Subject,
		Origen:       origen,
		ActivacionID: entrada.ActivacionId.String(),
	})
	if err != nil {
		escribirError(w, traducir(err))
		return
	}

	id, err := uuid.Parse(res.EmergenciaID)
	if err != nil {
		escribirError(w, traducir(err))
		return
	}
	estado := http.StatusCreated
	if res.Idempotente {
		estado = http.StatusOK
	}
	escribirJSON(w, estado, msgclient.CasoEmergencia{Id: id})
}

// agregarNota agrega una nota append-only al caso. Permitida mientras el caso no
// esté cerrado (ADR 21 §4). Texto ≤1000, recortado; vacío tras recorte → 400.
func (h Handlers) agregarNota(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	eid := r.PathValue("eid")

	var entrada msgclient.NotaEmergenciaEntrada
	if err := json.NewDecoder(r.Body).Decode(&entrada); err != nil {
		escribirError(w, platformerrors.NewValidacion("cuerpo inválido"))
		return
	}
	cuerpo := strings.TrimSpace(entrada.Cuerpo)
	if cuerpo == "" || len([]rune(cuerpo)) > 1000 {
		escribirError(w, platformerrors.NewValidacion("la nota debe tener de 1 a 1000 caracteres"))
		return
	}

	_, err := h.Emergencias.AgregarNota(r.Context(), emergencias.EntradaNota{
		TenantID:       claims.TenantID,
		EmergenciaID:   eid,
		ActorReal:      claims.Subject,
		AudienciaAutor: string(claims.Audiencia),
		Cuerpo:         cuerpo,
	})
	if err != nil {
		if errors.Is(err, emergencias.ErrCerrada) {
			escribirError(w, platformerrors.NewConflicto("emergencia_cerrada"))
			return
		}
		if errors.Is(err, emergencias.ErrNoEncontrada) {
			escribirError(w, platformerrors.NewNoEncontrado("emergencia no encontrada"))
			return
		}
		escribirError(w, traducir(err))
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// reclamar toma el reclamo del caso (abierta → en_atencion). Solo miembros del
// pool de Reacción (DEC-028). Conflicto de versión → 409.
func (h Handlers) reclamar(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	eid := r.PathValue("eid")

	if err := h.Autz.PuedeAtenderEmergencia(r.Context(), claims.TenantID, eid, claims); err != nil {
		escribirError(w, err)
		return
	}

	var entrada msgclient.VersionEsperadaEntrada
	if err := json.NewDecoder(r.Body).Decode(&entrada); err != nil {
		escribirError(w, platformerrors.NewValidacion("cuerpo inválido"))
		return
	}

	if _, err := h.Emergencias.Reclamar(r.Context(), claims.TenantID, eid, claims.Subject,
		msgtipos.VersionReclamo(entrada.VersionEsperada)); err != nil {
		escribirError(w, traducirConflicto(err))
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// liberar cierra el reclamo activo y devuelve el caso al pool. version_esperada
// obligatoria en el query. Conflicto → 409; sin reclamo → 404.
func (h Handlers) liberar(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	eid := r.PathValue("eid")
	rid := r.PathValue("rid")

	ver, err := strconv.ParseInt(r.URL.Query().Get("version_esperada"), 10, 64)
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("version_esperada inválida o faltante"))
		return
	}

	if err := h.Autz.PuedeAtenderEmergencia(r.Context(), claims.TenantID, eid, claims); err != nil {
		escribirError(w, err)
		return
	}

	if err := h.Emergencias.Liberar(r.Context(), claims.TenantID, eid, rid, claims.Subject,
		msgtipos.VersionReclamo(ver)); err != nil {
		if errors.Is(err, emergencias.ErrSinReclamo) {
			escribirError(w, platformerrors.NewNoEncontrado("reclamo no encontrado"))
			return
		}
		escribirError(w, traducirConflicto(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// transferir mueve la responsabilidad del caso a otro miembro de Reacción.
func (h Handlers) transferir(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	eid := r.PathValue("eid")

	if err := h.Autz.PuedeAtenderEmergencia(r.Context(), claims.TenantID, eid, claims); err != nil {
		escribirError(w, err)
		return
	}

	var entrada msgclient.TransferenciaEntrada
	if err := json.NewDecoder(r.Body).Decode(&entrada); err != nil {
		escribirError(w, platformerrors.NewValidacion("cuerpo inválido"))
		return
	}

	if err := h.Emergencias.Transferir(r.Context(), claims.TenantID, eid, claims.Subject,
		entrada.ResponsableDestinoId.String(), msgtipos.VersionReclamo(entrada.VersionEsperada)); err != nil {
		if errors.Is(err, emergencias.ErrSinReclamo) {
			escribirError(w, platformerrors.NewNoEncontrado("reclamo no encontrado"))
			return
		}
		escribirError(w, traducirConflicto(err))
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// resolver pasa el caso a resuelta (solo el responsable vigente, ADR 21 §5).
// Exige una nota de resolución 1..1000: se resuelve primero (valida estado y
// versión) y, en éxito, se registra la nota con es_resolucion=true.
func (h Handlers) resolver(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	eid := r.PathValue("eid")

	if err := h.Autz.PuedeAtenderEmergencia(r.Context(), claims.TenantID, eid, claims); err != nil {
		escribirError(w, err)
		return
	}

	var entrada msgclient.ResolucionEmergenciaEntrada
	if err := json.NewDecoder(r.Body).Decode(&entrada); err != nil {
		escribirError(w, platformerrors.NewValidacion("cuerpo inválido"))
		return
	}
	nota := strings.TrimSpace(entrada.Nota)
	if nota == "" || len([]rune(nota)) > 1000 {
		escribirError(w, platformerrors.NewValidacion("la nota de resolución debe tener de 1 a 1000 caracteres"))
		return
	}

	if err := h.Emergencias.Resolver(r.Context(), claims.TenantID, eid, claims.Subject,
		msgtipos.VersionReclamo(entrada.VersionEsperada)); err != nil {
		if errors.Is(err, emergencias.ErrTransicionInvalida) {
			escribirError(w, platformerrors.NewConflicto("transicion_invalida"))
			return
		}
		if errors.Is(err, emergencias.ErrSinReclamo) {
			escribirError(w, platformerrors.NewNoEncontrado("reclamo no encontrado"))
			return
		}
		escribirError(w, traducirConflicto(err))
		return
	}

	// La nota de resolución queda en el expediente. Si falla, el caso ya está
	// resuelto; se reporta el error pero la transición no se revierte (resuelta
	// es irreversible, ADR 21 §6).
	if _, err := h.Emergencias.AgregarNota(r.Context(), emergencias.EntradaNota{
		TenantID:       claims.TenantID,
		EmergenciaID:   eid,
		ActorReal:      claims.Subject,
		AudienciaAutor: string(claims.Audiencia),
		Cuerpo:         nota,
		EsResolucion:   true,
	}); err != nil {
		escribirError(w, traducir(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// cerrar cierra administrativamente el caso (resuelta → cerrada). Interno dentro
// del alcance o admin (ADR 21 §5); el cliente no cierra.
func (h Handlers) cerrar(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	if claims.Audiencia == auth.AudienciaCliente {
		escribirError(w, platformerrors.NewNoAutorizado("el cliente no cierra emergencias"))
		return
	}
	eid := r.PathValue("eid")

	if err := h.Emergencias.Cerrar(r.Context(), claims.TenantID, eid, claims.Subject); err != nil {
		if errors.Is(err, emergencias.ErrTransicionInvalida) {
			escribirError(w, platformerrors.NewConflicto("transicion_invalida"))
			return
		}
		if errors.Is(err, emergencias.ErrNoEncontrada) {
			escribirError(w, platformerrors.NewNoEncontrado("emergencia no encontrada"))
			return
		}
		escribirError(w, traducir(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// clienteDeClaims devuelve el cliente_id a persistir en el caso. Para una
// activación de cliente, es el Subject del token (el usuario-cliente, igual que
// conversacion.cliente_id se compara contra el sub). Para interno, mensajeria no
// conoce el cliente del viaje (lo proyecta de viaje.*); va vacío y se persiste
// NULL.
func clienteDeClaims(c *auth.Claims) string {
	if c.Audiencia == auth.AudienciaCliente {
		return c.Subject
	}
	return ""
}

// traducir conserva la categoría de un PlatformError y envuelve cualquier otro
// como error interno.
func traducir(err error) error {
	if platformerrors.AsCategory(err) != platformerrors.CategoryUnknown {
		return err
	}
	return platformerrors.NewRotoErr(err, "mensajeria: error interno")
}

// traducirConflicto mapea el conflicto de versión del dominio a 409
// conflicto_version_reclamo; cualquier otro error pasa por traducir.
func traducirConflicto(err error) error {
	var conflicto emergencias.ErrConflictoVersion
	if errors.As(err, &conflicto) {
		return platformerrors.NewConflicto("conflicto_version_reclamo")
	}
	return traducir(err)
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
