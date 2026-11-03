package mensajeria

import (
	"context"
	"errors"
	"strings"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-mensajeria/adapters/http/boundary"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/autorizacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/emergencias"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
	"github.com/google/uuid"
)

// EmergenciasAPI es la superficie pública en proceso del carril de emergencia
// (E7). La consume el adaptador en proceso bff→mensajeria de pf-svc-core, que es
// el único repo que ve a bff y a mensajeria a la vez. Sus tipos son los de
// pf-pkg-contracts, de modo que el adaptador la envuelva sin traducir.
//
// Es la ÚNICA copia de las reglas de contrato de emergencia (autorizar,
// proyectar por audiencia, traducir errores, orquestar resolver =
// Resolver + AgregarNota): el handler HTTP de adapters/http/emergencias solo
// decodifica, llama a esta superficie y codifica.
//
// Familias: las operaciones por viaje (tracer.17) y las gemelas por id del caso
// (tracer.34, DEC-028) corren sobre los MISMOS casos de uso con las MISMAS
// reglas; solo cambian los parámetros de ruta. Las por viaje no usan el viaje_id
// para autorizar: el caso se nombra por su id.
//
// Reglas (internal/autorizacion): sin acotar por el tenant de quien pregunta
// (DEC-129); Reacción por rol, sin filas de membresía (DEC-130); Supervisor y
// Coordinador ven, liberan y transfieren pero no reclaman ni resuelven (DEC-131);
// interno sin permiso → 403, Cliente sobre otra empresa → 404 (DEC-132).
//
// Autorización e identidad: lee los claims del ctx (auth.ClaimsFromContext), no
// de un bearer. El bff antepone su middleware, así que los claims ya viajan en el
// ctx cuando la llamada llega aquí en proceso.
type EmergenciasAPI interface {
	ListarEmergencias(ctx context.Context, viajeID msgclient.Id) (*msgclient.PaginaEmergencias, error)
	ActivarEmergencia(ctx context.Context, viajeID msgclient.Id, body msgclient.ActivarEmergenciaEntrada) (caso *msgclient.CasoEmergencia, idempotente bool, err error)
	AgregarNotaEmergencia(ctx context.Context, viajeID, emergenciaID msgclient.Id, body msgclient.NotaEmergenciaEntrada) error
	ReclamarEmergencia(ctx context.Context, viajeID, emergenciaID msgclient.Id, body msgclient.VersionEsperadaEntrada) (*msgclient.Reclamo, error)
	LiberarReclamoEmergencia(ctx context.Context, viajeID, emergenciaID, reclamoID msgclient.Id, versionEsperada msgclient.VersionReclamo) error
	TransferirEmergencia(ctx context.Context, viajeID, emergenciaID msgclient.Id, body msgclient.TransferenciaEntrada) (*msgclient.Reclamo, error)
	ResolverEmergencia(ctx context.Context, viajeID, emergenciaID msgclient.Id, body msgclient.ResolucionEmergenciaEntrada) error
	CerrarEmergencia(ctx context.Context, viajeID, emergenciaID msgclient.Id) error

	ListarBandejaEmergencias(ctx context.Context, params msgclient.ListarBandejaEmergenciasParams) (*msgclient.PaginaBandejaEmergencias, error)
	ObtenerEmergencia(ctx context.Context, emergenciaID msgclient.Id) (*msgclient.CasoEmergencia, error)
	ListarNotasEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, params msgclient.ListarNotasEmergenciaPorIdParams) (*msgclient.PaginaNotasEmergencia, error)
	ReclamarEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, body msgclient.VersionEsperadaEntrada) (*msgclient.Reclamo, error)
	LiberarReclamoEmergenciaPorId(ctx context.Context, emergenciaID, reclamoID msgclient.Id, params msgclient.LiberarReclamoEmergenciaPorIdParams) error
	TransferirEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, body msgclient.TransferenciaEntrada) (*msgclient.Reclamo, error)
	ResolverEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, body msgclient.ResolucionEmergenciaEntrada) (*msgclient.EmergenciaReaccion, error)
	CerrarEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id) (*msgclient.CasoEmergencia, error)
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
	TenantDeEmergencia(ctx context.Context, emergenciaID string, c *auth.Claims) (string, error)
	Emergencia(ctx context.Context, emergenciaID string, c *auth.Claims, p autorizacion.Permiso) (string, error)
}

// casoDeEmergencia es la parte de emergencias.Servicio que la fachada usa.
type casoDeEmergencia interface {
	Activar(ctx context.Context, in emergencias.EntradaActivacion) (emergencias.ResultadoActivacion, error)
	AgregarNota(ctx context.Context, in emergencias.EntradaNota) (string, error)
	Reclamar(ctx context.Context, tenantID, emergenciaID, actorReal string, esperada msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error)
	Liberar(ctx context.Context, tenantID, emergenciaID, reclamoID, actorReal string, esperada msgtipos.VersionReclamo, mando bool) error
	Transferir(ctx context.Context, tenantID, emergenciaID, actor, destino string, esperada msgtipos.VersionReclamo, mando bool) error
	Resolver(ctx context.Context, tenantID, emergenciaID, actorReal string, esperada msgtipos.VersionReclamo) error
	Cerrar(ctx context.Context, tenantID, emergenciaID, actorReal string) error
	ListarPorViaje(ctx context.Context, viajeID string) ([]emergencias.CasoProyectado, error)
	Bandeja(ctx context.Context) ([]emergencias.CasoProyectado, error)
	Obtener(ctx context.Context, tenantID, emergenciaID string) (emergencias.CasoProyectado, error)
	ListarNotas(ctx context.Context, tenantID, emergenciaID string) ([]emergencias.NotaProyectada, error)
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

// claimsDe lee los claims del ctx y rechaza su ausencia o un sujeto que no es
// UUID, igual que cada handler HTTP al inicio (boundary.Claims). En proceso los
// claims ya vienen del middleware del bff.
func claimsDe(ctx context.Context) (*auth.Claims, error) {
	c := boundary.Claims(ctx)
	if c == nil {
		return nil, platformerrors.NewNoAutorizado("mensajeria: sin claims")
	}
	return c, nil
}

// ListarEmergencias devuelve los casos del viaje. Al interno, todos y completos,
// de cualquier tenant (DEC-129). Al Cliente, solo los de su empresa y no
// cerrados, en la vista mínima (ADR 21 §6).
func (a *emergenciasAPI) ListarEmergencias(ctx context.Context, viajeID msgclient.Id) (*msgclient.PaginaEmergencias, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	casos, err := a.emergencias.ListarPorViaje(ctx, viajeID.String())
	if err != nil {
		return nil, traducir(err)
	}
	esCliente := claims.Audiencia == auth.AudienciaCliente
	items := make([]msgclient.CasoEmergencia, 0, len(casos))
	for _, c := range casos {
		if esCliente && (c.TenantID != claims.TenantID || c.Estado == string(msgclient.EstadoEmergenciaCerrada)) {
			continue
		}
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

// AgregarNotaEmergencia: el Cliente sobre un caso de su empresa o cualquier
// interno (ADR 21 §4), sin acotar por tenant al interno (DEC-129).
func (a *emergenciasAPI) AgregarNotaEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID msgclient.Id, body msgclient.NotaEmergenciaEntrada) error {
	claims, err := claimsDe(ctx)
	if err != nil {
		return err
	}
	cuerpo, err := notaValida(body.Cuerpo, "la nota debe tener de 1 a 1000 caracteres")
	if err != nil {
		return err
	}
	eid := emergenciaID.String()
	tenant, err := a.autz.TenantDeEmergencia(ctx, eid, claims)
	if err != nil {
		return err
	}
	_, err = a.emergencias.AgregarNota(ctx, emergencias.EntradaNota{
		TenantID:       tenant,
		EmergenciaID:   eid,
		ActorReal:      claims.Subject,
		AudienciaAutor: string(claims.Audiencia),
		Cuerpo:         cuerpo,
	})
	return traducirEmergencia(err)
}

func (a *emergenciasAPI) ReclamarEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID msgclient.Id, body msgclient.VersionEsperadaEntrada) (*msgclient.Reclamo, error) {
	return a.ReclamarEmergenciaPorId(ctx, emergenciaID, body)
}

func (a *emergenciasAPI) LiberarReclamoEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID, reclamoID msgclient.Id, versionEsperada msgclient.VersionReclamo) error {
	return a.LiberarReclamoEmergenciaPorId(ctx, emergenciaID, reclamoID,
		msgclient.LiberarReclamoEmergenciaPorIdParams{VersionEsperada: versionEsperada})
}

func (a *emergenciasAPI) TransferirEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID msgclient.Id, body msgclient.TransferenciaEntrada) (*msgclient.Reclamo, error) {
	return a.TransferirEmergenciaPorId(ctx, emergenciaID, body)
}

func (a *emergenciasAPI) ResolverEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID msgclient.Id, body msgclient.ResolucionEmergenciaEntrada) error {
	_, err := a.ResolverEmergenciaPorId(ctx, emergenciaID, body)
	return err
}

func (a *emergenciasAPI) CerrarEmergencia(ctx context.Context, _ msgclient.Id, emergenciaID msgclient.Id) error {
	_, err := a.CerrarEmergenciaPorId(ctx, emergenciaID)
	return err
}

// ListarBandejaEmergencias es la bandeja única de emergencias (DEC-130): la ven
// Reacción, Supervisor y Coordinador (DEC-131); otro rol y el Cliente reciben 404
// (no tienen bandeja). Excluye cerrados; del más antiguo al más reciente.
func (a *emergenciasAPI) ListarBandejaEmergencias(ctx context.Context, _ msgclient.ListarBandejaEmergenciasParams) (*msgclient.PaginaBandejaEmergencias, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	if claims.Audiencia == auth.AudienciaCliente || !autorizacion.VeBandejaEmergencias(claims.Rol) {
		return nil, platformerrors.NewNoEncontrado("el sujeto no tiene bandeja de emergencias")
	}
	casos, err := a.emergencias.Bandeja(ctx)
	if err != nil {
		return nil, traducir(err)
	}
	items := make([]msgclient.EmergenciaReaccion, 0, len(casos))
	for _, c := range casos {
		items = append(items, vistaReaccion(c))
	}
	return &msgclient.PaginaBandejaEmergencias{Items: items}, nil
}

// ObtenerEmergencia devuelve el caso completo, con viaje_id y cliente_id, para
// que el bff componga el detalle de Reacción.
func (a *emergenciasAPI) ObtenerEmergencia(ctx context.Context, emergenciaID msgclient.Id) (*msgclient.CasoEmergencia, error) {
	caso, err := a.leerCaso(ctx, emergenciaID, autorizacion.VerCaso)
	if err != nil {
		return nil, err
	}
	out := proyectar(caso, false)
	return &out, nil
}

// ListarNotasEmergenciaPorId: mismo acceso que ObtenerEmergencia; de la más
// antigua a la más reciente.
func (a *emergenciasAPI) ListarNotasEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, _ msgclient.ListarNotasEmergenciaPorIdParams) (*msgclient.PaginaNotasEmergencia, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	eid := emergenciaID.String()
	tenant, err := a.autz.Emergencia(ctx, eid, claims, autorizacion.VerCaso)
	if err != nil {
		return nil, err
	}
	notas, err := a.emergencias.ListarNotas(ctx, tenant, eid)
	if err != nil {
		return nil, traducir(err)
	}
	items := make([]msgclient.NotaEmergencia, 0, len(notas))
	for _, n := range notas {
		esResolucion := n.EsResolucion
		nota := msgclient.NotaEmergencia{
			Id:             uuidOCero(n.ID),
			EmergenciaId:   uuidOCero(n.EmergenciaID),
			AudienciaAutor: msgclient.Audiencia(n.AudienciaAutor),
			Cuerpo:         n.Cuerpo,
			EsResolucion:   &esResolucion,
			CreadoEn:       n.CreadoEn,
		}
		if id, err := uuid.Parse(n.ActorReal); err == nil {
			nota.ActorReal = &id
		}
		items = append(items, nota)
	}
	return &msgclient.PaginaNotasEmergencia{Items: items}, nil
}

// ReclamarEmergenciaPorId: solo Reacción (DEC-130, DEC-131) y solo si nadie
// tiene el caso (DEC-125). Devuelve el reclamo abierto.
func (a *emergenciasAPI) ReclamarEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, body msgclient.VersionEsperadaEntrada) (*msgclient.Reclamo, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	eid := emergenciaID.String()
	tenant, err := a.autz.Emergencia(ctx, eid, claims, autorizacion.AtenderCaso)
	if err != nil {
		return nil, err
	}
	nueva, err := a.emergencias.Reclamar(ctx, tenant, eid, claims.Subject, msgtipos.VersionReclamo(body.VersionEsperada))
	if err != nil {
		return nil, traducirEmergencia(err)
	}
	return a.reclamoVigente(ctx, tenant, eid, nueva)
}

// LiberarReclamoEmergenciaPorId: el responsable (Reacción) o cualquier
// Supervisor/Coordinador (DEC-126). Un miembro de Reacción que no es el
// responsable recibe 403 desde el dominio.
func (a *emergenciasAPI) LiberarReclamoEmergenciaPorId(ctx context.Context, emergenciaID, reclamoID msgclient.Id, params msgclient.LiberarReclamoEmergenciaPorIdParams) error {
	claims, err := claimsDe(ctx)
	if err != nil {
		return err
	}
	eid := emergenciaID.String()
	tenant, err := a.autz.Emergencia(ctx, eid, claims, autorizacion.MoverCaso)
	if err != nil {
		return err
	}
	err = a.emergencias.Liberar(ctx, tenant, eid, reclamoID.String(), claims.Subject,
		msgtipos.VersionReclamo(params.VersionEsperada), conMando(claims))
	return traducirEmergencia(err)
}

// TransferirEmergenciaPorId: mismo permiso que liberar (DEC-126). Devuelve el
// reclamo nuevo del destino.
func (a *emergenciasAPI) TransferirEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, body msgclient.TransferenciaEntrada) (*msgclient.Reclamo, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	eid := emergenciaID.String()
	tenant, err := a.autz.Emergencia(ctx, eid, claims, autorizacion.MoverCaso)
	if err != nil {
		return nil, err
	}
	// shortcut: no se verifica que el destino tenga rol `reaccion` (contrato,
	// DEC-130): mensajeria no conoce los roles de otros usuarios; se verifica
	// cuando haya proyección de identidad o el bff lo valide antes.
	err = a.emergencias.Transferir(ctx, tenant, eid, claims.Subject, body.ResponsableDestinoId.String(),
		msgtipos.VersionReclamo(body.VersionEsperada), conMando(claims))
	if err != nil {
		return nil, traducirEmergencia(err)
	}
	return a.reclamoVigente(ctx, tenant, eid, 0)
}

// ResolverEmergenciaPorId: solo el responsable vigente de Reacción (ADR 21 §5);
// Supervisor y Coordinador no resuelven (DEC-131). Nota obligatoria 1..1000; sin
// nota válida responde 400 y el caso no cambia. Devuelve la vista de Reacción.
func (a *emergenciasAPI) ResolverEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, body msgclient.ResolucionEmergenciaEntrada) (*msgclient.EmergenciaReaccion, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	eid := emergenciaID.String()
	tenant, err := a.autz.Emergencia(ctx, eid, claims, autorizacion.AtenderCaso)
	if err != nil {
		return nil, err
	}
	nota, err := notaValida(body.Nota, "la nota de resolución debe tener de 1 a 1000 caracteres")
	if err != nil {
		return nil, err
	}
	if err := a.emergencias.Resolver(ctx, tenant, eid, claims.Subject, msgtipos.VersionReclamo(body.VersionEsperada)); err != nil {
		return nil, traducirEmergencia(err)
	}
	// La nota de resolución queda en el expediente. Si falla, el caso ya está
	// resuelto (irreversible, ADR 21 §6): se reporta pero no se revierte.
	if _, err := a.emergencias.AgregarNota(ctx, emergencias.EntradaNota{
		TenantID:       tenant,
		EmergenciaID:   eid,
		ActorReal:      claims.Subject,
		AudienciaAutor: string(claims.Audiencia),
		Cuerpo:         nota,
		EsResolucion:   true,
	}); err != nil {
		return nil, traducir(err)
	}
	caso, err := a.emergencias.Obtener(ctx, tenant, eid)
	if err != nil {
		return nil, traducirEmergencia(err)
	}
	out := vistaReaccion(caso)
	return &out, nil
}

// CerrarEmergenciaPorId: monitoreo o Administrador (ADR 21 §5, E8-T18); nunca
// Reacción ni Ejecutivo (403). El alcance por viaje del monitoreo lo compone el
// bff (mensajeria no conoce la asignación de unidades).
func (a *emergenciasAPI) CerrarEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id) (*msgclient.CasoEmergencia, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	eid := emergenciaID.String()
	tenant, err := a.autz.Emergencia(ctx, eid, claims, autorizacion.CerrarCaso)
	if err != nil {
		return nil, err
	}
	if err := a.emergencias.Cerrar(ctx, tenant, eid, claims.Subject); err != nil {
		return nil, traducirEmergencia(err)
	}
	caso, err := a.emergencias.Obtener(ctx, tenant, eid)
	if err != nil {
		return nil, traducirEmergencia(err)
	}
	out := proyectar(caso, false)
	return &out, nil
}

// leerCaso autoriza el permiso y lee el caso.
func (a *emergenciasAPI) leerCaso(ctx context.Context, emergenciaID msgclient.Id, p autorizacion.Permiso) (emergencias.CasoProyectado, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return emergencias.CasoProyectado{}, err
	}
	eid := emergenciaID.String()
	tenant, err := a.autz.Emergencia(ctx, eid, claims, p)
	if err != nil {
		return emergencias.CasoProyectado{}, err
	}
	caso, err := a.emergencias.Obtener(ctx, tenant, eid)
	return caso, traducirEmergencia(err)
}

// reclamoVigente hidrata el reclamo activo tras reclamar o transferir. Se lee
// aparte, como en conversación; si una liberación concurrente ya lo cerró, se
// devuelve solo la versión conocida (0 si no se conoce).
func (a *emergenciasAPI) reclamoVigente(ctx context.Context, tenant, eid string, version msgtipos.VersionReclamo) (*msgclient.Reclamo, error) {
	caso, err := a.emergencias.Obtener(ctx, tenant, eid)
	if err != nil {
		return nil, traducirEmergencia(err)
	}
	if r := reclamoContrato(caso); r != nil {
		return r, nil
	}
	return &msgclient.Reclamo{Version: int64(version)}, nil
}

// proyectar mapea un caso leído al CasoEmergencia del contrato. Para audiencia
// Cliente oculta los campos internos (origen, actor_real y reclamo_activo quedan
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
	caso.Origen = msgclient.OrigenActivacion(c.Origen)
	if id, err := uuid.Parse(c.ActorReal); err == nil {
		caso.ActorReal = &id
	}
	caso.ReclamoActivo = reclamoContrato(c)
	return caso
}

// vistaReaccion mapea un caso a EmergenciaReaccion: sin viaje, cliente, actor ni
// notas (DEC-121 #2).
func vistaReaccion(c emergencias.CasoProyectado) msgclient.EmergenciaReaccion {
	return msgclient.EmergenciaReaccion{
		Id:             uuidOCero(c.ID),
		Estado:         msgclient.EstadoEmergencia(c.Estado),
		Origen:         msgclient.OrigenActivacion(c.Origen),
		VersionReclamo: int64(c.VersionReclamo),
		ReclamoActivo:  reclamoContrato(c),
		ActivadaEn:     c.ActivadaEn,
		ResueltaEn:     c.ResueltaEn,
		CerradaEn:      c.CerradaEn,
	}
}

// reclamoContrato mapea el reclamo activo al Reclamo del contrato, o nil si el
// caso está en el pool. El esquema Reclamo es el de conversación: su
// conversacion_id lleva el id del caso, el agregado del reclamo de emergencia.
func reclamoContrato(c emergencias.CasoProyectado) *msgclient.Reclamo {
	if c.Reclamo == nil {
		return nil
	}
	return &msgclient.Reclamo{
		Id:             uuidOCero(c.Reclamo.ID),
		ConversacionId: uuidOCero(c.ID),
		ResponsableId:  uuidOCero(c.Reclamo.Responsable),
		Version:        int64(c.Reclamo.Version),
		AbiertoEn:      c.Reclamo.AbiertoEn,
	}
}

func uuidOCero(s string) msgclient.Id {
	id, _ := uuid.Parse(s)
	return id
}

// notaValida recorta la nota y exige de 1 a 1000 caracteres (ADR 21 §4).
func notaValida(texto, mensaje string) (string, error) {
	nota := strings.TrimSpace(texto)
	if nota == "" || len([]rune(nota)) > 1000 {
		return "", platformerrors.NewValidacion("%s", mensaje)
	}
	return nota, nil
}

// clienteDeClaims devuelve el cliente_id a persistir en el caso: para una
// activación de cliente, su empresa, que es el tenant del token (DEC-112,
// DEC-124); el bff la usa para componer desde catalogos. Para interno va vacío
// (NULL): mensajeria no conoce el cliente del viaje. Un tenant que no es UUID
// también va vacío: la activación de una emergencia no falla por el formato de
// un claim.
func clienteDeClaims(c *auth.Claims) string {
	if _, err := uuid.Parse(c.TenantID); c.Audiencia != auth.AudienciaCliente || err != nil {
		return ""
	}
	return c.TenantID
}

// traducir conserva la categoría de un PlatformError y envuelve cualquier otro
// como error interno.
func traducir(err error) error {
	if platformerrors.AsCategory(err) != platformerrors.CategoryUnknown {
		return err
	}
	return platformerrors.NewRotoErr(err, "mensajeria: error interno")
}

// conMando: un Supervisor o Coordinador libera o transfiere el caso sin ser su
// responsable (DEC-126).
func conMando(c *auth.Claims) bool {
	return c.Audiencia == auth.AudienciaInterno && emergencias.TieneMando(c.Rol)
}

// traducirEmergencia mapea los errores del dominio de emergencia a la taxonomía:
// conflicto de versión → 409 conflicto_version_reclamo; transición inválida →
// 409; caso cerrado → 409 emergencia_cerrada; sin reclamo o caso inexistente →
// 404; no es el responsable → 403 (DEC-126). nil pasa como nil.
func traducirEmergencia(err error) error {
	var conflicto emergencias.ErrConflictoVersion
	switch {
	case err == nil:
		return nil
	case errors.As(err, &conflicto):
		return platformerrors.NewConflicto("conflicto_version_reclamo")
	case errors.Is(err, emergencias.ErrTransicionInvalida):
		return platformerrors.NewConflicto("transicion_invalida")
	case errors.Is(err, emergencias.ErrCerrada):
		return platformerrors.NewConflicto("emergencia_cerrada")
	case errors.Is(err, emergencias.ErrSinReclamo):
		return platformerrors.NewNoEncontrado("reclamo no encontrado")
	case errors.Is(err, emergencias.ErrNoEncontrada):
		return platformerrors.NewNoEncontrado("emergencia no encontrada")
	case errors.Is(err, emergencias.ErrNoEsResponsable):
		return platformerrors.NewProhibido("el actor no es el responsable del caso")
	}
	return traducir(err)
}
