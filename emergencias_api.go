package mensajeria

import (
	"context"
	"errors"
	"strings"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/autorizacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/emergencias"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
	"github.com/google/uuid"
)

// EmergenciasAPI es la superficie pública en proceso del carril de emergencia
// (E7). La consume el adaptador en proceso bff→mensajeria de pf-svc-core, que es
// el único repo que ve a bff y a mensajeria a la vez. Su forma es exactamente el
// puerto bff.MensajeriaPort (tipos de pf-pkg-contracts), de modo que el adaptador
// la envuelva sin traducir.
//
// Por qué existe (00/05/09 decían bff→mensajeria SIEMPRE HTTP, sin Consultas()):
// al co-ubicar mensajeria dentro de core (E7 en el deployment compuesto), la
// arista se resuelve en proceso y necesita una superficie pública tipada con el
// contrato, igual que operacion.ConsultasAPI y catalogos.ConsultasAPI. La lógica
// de contrato (autorizar, proyectar por audiencia, traducir errores, orquestar
// resolver=Resolver+AgregarNota) vive AQUÍ, en una sola copia; el handler HTTP de
// adapters/http/emergencias delega en esta misma superficie (minimize reader
// load: la regla de audiencia no se duplica).
//
// Autorización e identidad: igual que el handler HTTP, lee los claims del ctx
// (auth.ClaimsFromContext), no de un bearer. El bff antepone su middleware, así
// que los claims ya viajan en el ctx cuando la llamada llega aquí en proceso. El
// parámetro bearer del puerto bff lo consume el adaptador HTTP del bff, no esta
// superficie.
type EmergenciasAPI interface {
	ListarEmergencias(ctx context.Context, viajeID msgclient.Id) (*msgclient.PaginaEmergencias, error)
	ActivarEmergencia(ctx context.Context, viajeID msgclient.Id, body msgclient.ActivarEmergenciaEntrada) (caso *msgclient.CasoEmergencia, idempotente bool, err error)
	AgregarNotaEmergencia(ctx context.Context, viajeID, emergenciaID msgclient.Id, body msgclient.NotaEmergenciaEntrada) error
	ReclamarEmergencia(ctx context.Context, viajeID, emergenciaID msgclient.Id, body msgclient.VersionEsperadaEntrada) (*msgclient.Reclamo, error)
	LiberarReclamoEmergencia(ctx context.Context, viajeID, emergenciaID, reclamoID msgclient.Id, versionEsperada msgclient.VersionReclamo) error
	TransferirEmergencia(ctx context.Context, viajeID, emergenciaID msgclient.Id, body msgclient.TransferenciaEntrada) (*msgclient.Reclamo, error)
	ResolverEmergencia(ctx context.Context, viajeID, emergenciaID msgclient.Id, body msgclient.ResolucionEmergenciaEntrada) error
	CerrarEmergencia(ctx context.Context, viajeID, emergenciaID msgclient.Id) error
}

// Emergencias construye la superficie pública de emergencia sobre los casos de
// uso que el Module ya tiene (m.emergencias y m.autz). No toca New ni abre
// recursos: es una fachada sobre lo ya construido.
func (m *Module) Emergencias() EmergenciasAPI {
	return &emergenciasAPI{emergencias: m.emergencias, autz: m.autz}
}

// autorizador es la parte de autorizacion.Servicio que el carril de emergencia
// usa. Se declara aquí (consumidor) para no acoplar la fachada al tipo concreto.
type autorizador interface {
	PuedeAtenderEmergencia(ctx context.Context, tenantID, emergenciaID string, c *auth.Claims) error
}

// casoDeEmergencia es la parte de emergencias.Servicio que la fachada usa.
type casoDeEmergencia interface {
	Activar(ctx context.Context, in emergencias.EntradaActivacion) (emergencias.ResultadoActivacion, error)
	AgregarNota(ctx context.Context, in emergencias.EntradaNota) (string, error)
	Reclamar(ctx context.Context, tenantID, emergenciaID, actorReal string, esperada msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error)
	Liberar(ctx context.Context, tenantID, emergenciaID, reclamoID, actorReal string, esperada msgtipos.VersionReclamo) error
	Transferir(ctx context.Context, tenantID, emergenciaID, origen, destino string, esperada msgtipos.VersionReclamo) error
	Resolver(ctx context.Context, tenantID, emergenciaID, actorReal string, esperada msgtipos.VersionReclamo) error
	Cerrar(ctx context.Context, tenantID, emergenciaID, actorReal string) error
	ListarPorViaje(ctx context.Context, tenantID, viajeID string, soloActivos bool) ([]emergencias.CasoProyectado, error)
}

type emergenciasAPI struct {
	emergencias casoDeEmergencia
	autz        autorizador
}

// Las implementaciones reales satisfacen las interfaces de consumidor de la
// fachada (comprobación en compilación).
var (
	_ casoDeEmergencia = (*emergencias.Servicio)(nil)
	_ autorizador      = (*autorizacion.Servicio)(nil)
)

// claimsDe lee los claims del ctx y rechaza su ausencia, igual que cada handler
// HTTP al inicio. En proceso los claims ya vienen del middleware del bff.
func claimsDe(ctx context.Context) (*auth.Claims, error) {
	c := auth.ClaimsFromContext(ctx)
	if c == nil {
		return nil, platformerrors.NewNoAutorizado("mensajeria: sin claims")
	}
	return c, nil
}

func (a *emergenciasAPI) ListarEmergencias(ctx context.Context, viajeID msgclient.Id) (*msgclient.PaginaEmergencias, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	esCliente := claims.Audiencia == auth.AudienciaCliente
	casos, err := a.emergencias.ListarPorViaje(ctx, claims.TenantID, viajeID.String(), esCliente)
	if err != nil {
		return nil, traducir(err)
	}
	items := make([]msgclient.CasoEmergencia, 0, len(casos))
	for _, c := range casos {
		items = append(items, proyectar(c, esCliente))
	}
	return &msgclient.PaginaEmergencias{Items: items}, nil
}

func (a *emergenciasAPI) ActivarEmergencia(ctx context.Context, viajeID msgclient.Id, body msgclient.ActivarEmergenciaEntrada) (*msgclient.CasoEmergencia, bool, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, false, err
	}
	origen := msgclient.OrigenActivacionOperacion
	if claims.Audiencia == auth.AudienciaCliente {
		origen = msgclient.OrigenActivacionCliente
	}
	res, err := a.emergencias.Activar(ctx, emergencias.EntradaActivacion{
		TenantID:     claims.TenantID,
		ViajeID:      viajeID.String(),
		ClienteID:    clienteDeClaims(claims),
		ActorReal:    claims.Subject,
		Origen:       origen,
		ActivacionID: body.ActivacionId.String(),
	})
	if err != nil {
		return nil, false, traducir(err)
	}
	id, err := uuid.Parse(res.EmergenciaID)
	if err != nil {
		return nil, false, traducir(err)
	}
	return &msgclient.CasoEmergencia{Id: id}, res.Idempotente, nil
}

func (a *emergenciasAPI) AgregarNotaEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID msgclient.Id, body msgclient.NotaEmergenciaEntrada) error {
	claims, err := claimsDe(ctx)
	if err != nil {
		return err
	}
	cuerpo := strings.TrimSpace(body.Cuerpo)
	if cuerpo == "" || len([]rune(cuerpo)) > 1000 {
		return platformerrors.NewValidacion("la nota debe tener de 1 a 1000 caracteres")
	}
	_, err = a.emergencias.AgregarNota(ctx, emergencias.EntradaNota{
		TenantID:       claims.TenantID,
		EmergenciaID:   emergenciaID.String(),
		ActorReal:      claims.Subject,
		AudienciaAutor: string(claims.Audiencia),
		Cuerpo:         cuerpo,
	})
	if errors.Is(err, emergencias.ErrCerrada) {
		return platformerrors.NewConflicto("emergencia_cerrada")
	}
	if errors.Is(err, emergencias.ErrNoEncontrada) {
		return platformerrors.NewNoEncontrado("emergencia no encontrada")
	}
	if err != nil {
		return traducir(err)
	}
	return nil
}

func (a *emergenciasAPI) ReclamarEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID msgclient.Id, body msgclient.VersionEsperadaEntrada) (*msgclient.Reclamo, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	eid := emergenciaID.String()
	if err := a.autz.PuedeAtenderEmergencia(ctx, claims.TenantID, eid, claims); err != nil {
		return nil, err
	}
	nueva, err := a.emergencias.Reclamar(ctx, claims.TenantID, eid, claims.Subject, msgtipos.VersionReclamo(body.VersionEsperada))
	if err != nil {
		return nil, traducirConflicto(err)
	}
	// shortcut: el dominio solo devuelve la versión nueva, no un Reclamo completo;
	// el carril HTTP de mensajeria tampoco lo devolvía (201 sin cuerpo). Se
	// devuelve el Reclamo con Version poblada (lo que el panel usa para la
	// siguiente llamada optimista) y el resto en cero. Upgrade: si el panel llega
	// a necesitar id/responsable/timestamps, el dominio debe exponer una lectura
	// del reclamo vigente.
	return &msgclient.Reclamo{Version: int64(nueva)}, nil
}

func (a *emergenciasAPI) LiberarReclamoEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID, reclamoID msgclient.Id, versionEsperada msgclient.VersionReclamo) error {
	claims, err := claimsDe(ctx)
	if err != nil {
		return err
	}
	eid := emergenciaID.String()
	if err := a.autz.PuedeAtenderEmergencia(ctx, claims.TenantID, eid, claims); err != nil {
		return err
	}
	err = a.emergencias.Liberar(ctx, claims.TenantID, eid, reclamoID.String(), claims.Subject, msgtipos.VersionReclamo(versionEsperada))
	if errors.Is(err, emergencias.ErrSinReclamo) {
		return platformerrors.NewNoEncontrado("reclamo no encontrado")
	}
	if err != nil {
		return traducirConflicto(err)
	}
	return nil
}

func (a *emergenciasAPI) TransferirEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID msgclient.Id, body msgclient.TransferenciaEntrada) (*msgclient.Reclamo, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	eid := emergenciaID.String()
	if err := a.autz.PuedeAtenderEmergencia(ctx, claims.TenantID, eid, claims); err != nil {
		return nil, err
	}
	err = a.emergencias.Transferir(ctx, claims.TenantID, eid, claims.Subject, body.ResponsableDestinoId.String(), msgtipos.VersionReclamo(body.VersionEsperada))
	if errors.Is(err, emergencias.ErrSinReclamo) {
		return nil, platformerrors.NewNoEncontrado("reclamo no encontrado")
	}
	if err != nil {
		return nil, traducirConflicto(err)
	}
	// shortcut: el dominio de transferencia devuelve solo error; el carril HTTP
	// devolvía 201 sin cuerpo. Se devuelve un Reclamo vacío, misma semántica.
	// Upgrade: igual que ReclamarEmergencia, si el panel necesita el reclamo real.
	return &msgclient.Reclamo{}, nil
}

func (a *emergenciasAPI) ResolverEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID msgclient.Id, body msgclient.ResolucionEmergenciaEntrada) error {
	claims, err := claimsDe(ctx)
	if err != nil {
		return err
	}
	eid := emergenciaID.String()
	if err := a.autz.PuedeAtenderEmergencia(ctx, claims.TenantID, eid, claims); err != nil {
		return err
	}
	nota := strings.TrimSpace(body.Nota)
	if nota == "" || len([]rune(nota)) > 1000 {
		return platformerrors.NewValidacion("la nota de resolución debe tener de 1 a 1000 caracteres")
	}
	err = a.emergencias.Resolver(ctx, claims.TenantID, eid, claims.Subject, msgtipos.VersionReclamo(body.VersionEsperada))
	if errors.Is(err, emergencias.ErrTransicionInvalida) {
		return platformerrors.NewConflicto("transicion_invalida")
	}
	if errors.Is(err, emergencias.ErrSinReclamo) {
		return platformerrors.NewNoEncontrado("reclamo no encontrado")
	}
	if err != nil {
		return traducirConflicto(err)
	}
	// La nota de resolución queda en el expediente. Si falla, el caso ya está
	// resuelto (irreversible, ADR 21 §6): se reporta pero no se revierte.
	if _, err := a.emergencias.AgregarNota(ctx, emergencias.EntradaNota{
		TenantID:       claims.TenantID,
		EmergenciaID:   eid,
		ActorReal:      claims.Subject,
		AudienciaAutor: string(claims.Audiencia),
		Cuerpo:         nota,
		EsResolucion:   true,
	}); err != nil {
		return traducir(err)
	}
	return nil
}

func (a *emergenciasAPI) CerrarEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID msgclient.Id) error {
	claims, err := claimsDe(ctx)
	if err != nil {
		return err
	}
	if claims.Audiencia == auth.AudienciaCliente {
		return platformerrors.NewNoAutorizado("el cliente no cierra emergencias")
	}
	err = a.emergencias.Cerrar(ctx, claims.TenantID, emergenciaID.String(), claims.Subject)
	if errors.Is(err, emergencias.ErrTransicionInvalida) {
		return platformerrors.NewConflicto("transicion_invalida")
	}
	if errors.Is(err, emergencias.ErrNoEncontrada) {
		return platformerrors.NewNoEncontrado("emergencia no encontrada")
	}
	if err != nil {
		return traducir(err)
	}
	return nil
}

// proyectar mapea un caso leído al CasoEmergencia del contrato. Para audiencia
// Cliente oculta los campos internos (origen, actor_real quedan en su cero), que
// solo se entregan a Interno (ADR 21 §3, §6).
//
// shortcut: proyectar/uuidOCero/traducir/traducirConflicto/clienteDeClaims están
// duplicadas con adapters/http/emergencias (funciones puras idénticas). No se
// dedupe aquí porque el handler HTTP vive en un paquete que el root importa
// (Routes), así que no puede importar el root sin ciclo; extraerlas a
// internal/emergencias mete auth en el dominio y reescribe el handler de un
// carril de seguridad sin tests de forma. Upgrade: si la regla de audiencia
// cambia, extraer ProyectarCaso/Traducir a internal/emergencias y que ambos
// llamadores la usen.
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

// clienteDeClaims devuelve el cliente_id a persistir en el caso. Para una
// activación de cliente es el Subject del token; para interno va vacío (NULL).
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
