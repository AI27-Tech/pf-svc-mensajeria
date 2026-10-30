// Package conversacion contiene los handlers HTTP ENTRANTES de la conversación
// de negocio (arista bff → mensajeria, siempre HTTP; addendum §5). Monta el
// reclamo (reclamar, liberar, transferir), el envío de mensajes (EnviarMensaje,
// con alias_mostrado resuelto desde catalogos, DEC-097) y la LECTURA de la
// conversación (obtenerConversacion, listarMensajes) más marcarAtendida (E9-T8),
// todo detrás de la guarda de internal/autorizacion (DEC-097).
//
// Proyección por audiencia (contrato Conversacion/Mensaje; ADR 21 §6, patrón de
// emergencias): un lector Cliente recibe la vista mínima (sin actor_real,
// reclamo ni bandeja), con el estado atendido/resuelto de la conversación; un
// lector Interno recibe la vista completa. El dominio entrega todos los campos y
// esta frontera oculta los internos según la Audiencia del token (DEC-054).
//
// Los tipos de request/response son los generados de pf-pkg-contracts (tracer.21),
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

// Enviador envía un mensaje, hidrata el Mensaje del contrato, resuelve el
// cliente de la conversación para el alias, lee la conversación y su historia, y
// prende el indicador de atendida (internal/conversacion).
type Enviador interface {
	Enviar(ctx context.Context, in conversacion.EntradaEnvio) (conversacion.ResultadoEnvio, error)
	MensajePorID(ctx context.Context, tenantID, mensajeID string) (*msgclient.Mensaje, error)
	ClienteDeConversacion(ctx context.Context, tenantID, conversacionID string) (string, error)
	ObtenerConversacion(ctx context.Context, tenantID, conversacionID string) (*conversacion.ConversacionProyectada, error)
	ListarMensajes(ctx context.Context, in conversacion.EntradaListado) (conversacion.ResultadoListado, error)
	MarcarAtendida(ctx context.Context, tenantID, conversacionID, actorReal string, esperada msgtipos.VersionReclamo) error
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
	mux.HandleFunc("GET /api/v1/conversaciones/{id}", h.obtener)
	mux.HandleFunc("GET /api/v1/conversaciones/{id}/mensajes", h.listarMensajes)
	mux.HandleFunc("POST /api/v1/conversaciones/{id}/reclamos", h.reclamar)
	mux.HandleFunc("DELETE /api/v1/conversaciones/{id}/reclamos/{rid}", h.liberar)
	mux.HandleFunc("POST /api/v1/conversaciones/{id}/transferencias", h.transferir)
	mux.HandleFunc("POST /api/v1/conversaciones/{id}/mensajes", h.enviar)
	mux.HandleFunc("POST /api/v1/conversaciones/{id}/atendida", h.marcarAtendida)
}

// obtener devuelve el detalle de una conversación para el bootstrap (contrato
// obtenerConversacion). Autoriza con la guarda de acceso (DEC-097) y proyecta por
// audiencia: Cliente recibe la vista mínima (sin bandeja ni reclamo activo),
// Interno la vista completa. Poseer el UUID no autoriza (Addendum §7): sin acceso
// efectivo responde 404, igual que si no existiera (no se revela existencia por
// el código), así que el no_autorizado de la guarda se traduce a 404 en lectura.
func (h Handlers) obtener(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	convID := r.PathValue("id")

	if err := h.Autz.PuedeEscribir(r.Context(), claims.TenantID, convID, claims); err != nil {
		escribirError(w, ocultarAccesoEnLectura(err))
		return
	}

	c, err := h.Conversacion.ObtenerConversacion(r.Context(), claims.TenantID, convID)
	if err != nil {
		if errors.Is(err, conversacion.ErrConversacionNoEncontrada) {
			escribirError(w, platformerrors.NewNoEncontrado("conversacion no encontrada"))
			return
		}
		escribirError(w, traducir(err))
		return
	}

	escribirJSON(w, http.StatusOK, proyectarConversacion(c, claims.Audiencia == auth.AudienciaCliente))
}

// listarMensajes devuelve la historia por secuencia canónica de dominio
// (contrato listarMensajes). antes_de/despues_de son mutuamente excluyentes y se
// rechazan juntos con 400; sin ninguno, los más recientes. Cada Mensaje se
// proyecta por audiencia: el Cliente no ve actor_real ni rol_al_enviar, pero sí
// audiencia_autor (el "de_cliente": quién escribió, lado cliente o AI27).
func (h Handlers) listarMensajes(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		escribirError(w, platformerrors.NewNoAutorizado("sin claims"))
		return
	}
	convID := r.PathValue("id")

	if err := h.Autz.PuedeEscribir(r.Context(), claims.TenantID, convID, claims); err != nil {
		escribirError(w, ocultarAccesoEnLectura(err))
		return
	}

	in := conversacion.EntradaListado{TenantID: claims.TenantID, ConversacionID: convID}
	antes, hayAntes, err := secuenciaQuery(r, "antes_de")
	if err != nil {
		escribirError(w, err)
		return
	}
	despues, hayDespues, err := secuenciaQuery(r, "despues_de")
	if err != nil {
		escribirError(w, err)
		return
	}
	if hayAntes && hayDespues {
		escribirError(w, platformerrors.NewValidacion("antes_de y despues_de son mutuamente excluyentes"))
		return
	}
	if hayAntes {
		in.AntesDe = &antes
	}
	if hayDespues {
		in.DespuesDe = &despues
	}
	if lim := r.URL.Query().Get("limite"); lim != "" {
		v, err := strconv.Atoi(lim)
		if err != nil || v < 1 || v > 200 {
			escribirError(w, platformerrors.NewValidacion("limite inválido (1..200)"))
			return
		}
		in.Limite = v
	}

	res, err := h.Conversacion.ListarMensajes(r.Context(), in)
	if err != nil {
		if errors.Is(err, conversacion.ErrConversacionNoEncontrada) {
			escribirError(w, platformerrors.NewNoEncontrado("conversacion no encontrada"))
			return
		}
		escribirError(w, traducir(err))
		return
	}

	esCliente := claims.Audiencia == auth.AudienciaCliente
	items := make([]msgclient.Mensaje, 0, len(res.Mensajes))
	for _, m := range res.Mensajes {
		items = append(items, proyectarMensaje(m, esCliente))
	}
	escribirJSON(w, http.StatusOK, msgclient.PaginaMensajes{Items: items, HayMas: res.HayMas})
}

// marcarAtendida prende el indicador de "atendida" que verá el cliente (DEC-014,
// Q113). Solo el responsable del reclamo activo, con la versión esperada
// (concurrencia optimista). Autoriza con la misma guarda de escritura de bandeja
// (DEC-097); además el dominio exige que el actor sea el responsable del reclamo.
// Conflicto de versión → 409 conflicto_version_reclamo; sin reclamo propio → 404.
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

	err := h.Conversacion.MarcarAtendida(r.Context(), claims.TenantID, convID, claims.Subject,
		msgtipos.VersionReclamo(entrada.VersionEsperada))
	if err != nil {
		var conflicto conversacion.ErrConflictoVersionAtendida
		if errors.As(err, &conflicto) {
			// Misma limitación de código de máquina que el envío interno: la
			// plataforma solo expone la categoría (conflicto); el contrato declara
			// conflicto_version_reclamo.
			escribirError(w, platformerrors.NewConflicto("conflicto_version_reclamo"))
			return
		}
		if errors.Is(err, conversacion.ErrNoEsResponsable) {
			escribirError(w, platformerrors.NewNoEncontrado("reclamo no encontrado"))
			return
		}
		if errors.Is(err, conversacion.ErrConversacionNoEncontrada) {
			escribirError(w, platformerrors.NewNoEncontrado("conversacion no encontrada"))
			return
		}
		escribirError(w, traducir(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// proyectarConversacion mapea la conversación de dominio al Conversacion del
// contrato. Para audiencia Cliente oculta los campos internos (bandeja_id y
// reclamo_activo quedan nil), que solo se entregan a Interno (contrato
// Conversacion). version_reclamo y el estado atendido/resuelto SÍ se entregan al
// cliente (atendida_en es la marca que ve, DEC-014).
func proyectarConversacion(c *conversacion.ConversacionProyectada, esCliente bool) msgclient.Conversacion {
	out := msgclient.Conversacion{
		Id:              uuid.MustParse(c.ID),
		ViajeId:         uuid.MustParse(c.ViajeID),
		Estado:          msgclient.EstadoConversacion(c.Estado),
		EscrituraHasta:  c.EscrituraHasta,
		UltimaSecuencia: int64(c.UltimaSecuencia),
		VersionReclamo:  int64(c.VersionReclamo),
		AtendidaEn:      c.AtendidaEn,
		AbiertaEn:       c.AbiertaEn,
	}
	if c.ClienteID != "" {
		out.ClienteId = uuid.MustParse(c.ClienteID)
	}
	if esCliente {
		return out
	}
	// Interno ve los campos internos: bandeja de la ruta activa y reclamo activo.
	if c.BandejaID != "" {
		if id, err := uuid.Parse(c.BandejaID); err == nil {
			out.BandejaId = &id
		}
	}
	out.ReclamoActivo = c.Reclamo
	return out
}

// proyectarMensaje mapea un Mensaje hidratado a lo que su audiencia puede ver.
// Para audiencia Cliente oculta actor_real y rol_al_enviar (solo Interno, contrato
// Mensaje); audiencia_autor (el lado cliente vs. AI27) se conserva, es lo que el
// cliente necesita para saber quién escribió.
func proyectarMensaje(m msgclient.Mensaje, esCliente bool) msgclient.Mensaje {
	if esCliente {
		m.ActorReal = nil
		m.RolAlEnviar = nil
	}
	return m
}

// secuenciaQuery lee un parámetro de secuencia (antes_de/despues_de) del query.
// Devuelve el valor, si estaba presente, y un error de validación si estaba
// presente pero mal formado.
func secuenciaQuery(r *http.Request, nombre string) (int64, bool, error) {
	raw := r.URL.Query().Get(nombre)
	if raw == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, platformerrors.NewValidacion("%s inválido", nombre)
	}
	return v, true, nil
}

// ocultarAccesoEnLectura traduce el no_autorizado de la guarda de acceso a
// no_encontrado en las LECTURAS: poseer el UUID no autoriza y el servidor no
// revela existencia por el código (contrato obtenerConversacion/listarMensajes:
// "sin acceso efectivo responde 404"). El no_encontrado de la guarda (la
// conversación no existe para el tenant) ya es 404 y pasa sin cambio; cualquier
// otra categoría se conserva.
func ocultarAccesoEnLectura(err error) error {
	if platformerrors.AsCategory(err) == platformerrors.CategoryNoAutorizado {
		return platformerrors.NewNoEncontrado("conversacion no encontrada")
	}
	return err
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

	var versionEsperada *int64
	if entrada.VersionReclamoEsperada != nil {
		v := int64(*entrada.VersionReclamoEsperada)
		versionEsperada = &v
	}
	motivoOverride := ""
	if entrada.MotivoOverride != nil {
		motivoOverride = *entrada.MotivoOverride
	}

	res, err := h.Conversacion.Enviar(r.Context(), conversacion.EntradaEnvio{
		TenantID:               claims.TenantID,
		ConversacionID:         convID,
		ActorReal:              claims.Subject,
		ClientMessageID:        entrada.ClientMessageId.String(),
		Cuerpo:                 entrada.Cuerpo,
		AliasMostrado:          alias,
		RolAlEnviar:            claims.Rol,
		VersionReclamoEsperada: versionEsperada,
		MotivoOverride:         motivoOverride,
	})
	if err != nil {
		// Conversación no `abierta` (solo_lectura/cerrada): 409, como el conflicto
		// de versión de reclamo. El contrato lo marca con `codigo`
		// conversacion_solo_lectura; la taxonomía de la plataforma solo expone el
		// código de categoría (`conflicto`), limitación transversal igual que en
		// conflicto_version_reclamo (no se resuelve en esta historia).
		if errors.Is(err, conversacion.ErrConversacionSoloLectura) {
			escribirError(w, platformerrors.NewConflicto("conversacion_solo_lectura"))
			return
		}
		// Envío interno sin reclamo activo en la versión esperada: 409
		// conflicto_version_reclamo (Addendum §7). Misma limitación del código de
		// máquina que arriba: se emite la categoría conflicto.
		if errors.Is(err, conversacion.ErrSinReclamoActivo) {
			escribirError(w, platformerrors.NewConflicto("conflicto_version_reclamo"))
			return
		}
		// Override de supervisor no implementado en V1 (cerrado hasta que exista la
		// proyección de alcance jerárquico). Se rechaza como petición inválida: el
		// campo motivo_override no es procesable en esta versión. 400 está en el
		// contrato de enviarMensaje; no se inventa un 501 fuera de contrato.
		if errors.Is(err, conversacion.ErrOverrideNoImplementado) {
			escribirError(w, platformerrors.NewValidacion("motivo_override no está disponible en V1"))
			return
		}
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
