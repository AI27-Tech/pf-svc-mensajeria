package mensajeria

import (
	"context"
	"errors"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	bandejashttp "github.com/AI27-Tech/pf-svc-mensajeria/adapters/http/bandejas"
	convhttp "github.com/AI27-Tech/pf-svc-mensajeria/adapters/http/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/autorizacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
)

// ConversacionesAPI es la superficie pública en proceso de la conversación y la
// bandeja (E9-T8 lectura/envío, E8-T15 bandeja y reclamo). La consume el
// adaptador en proceso bff→mensajeria de pf-svc-core, igual que EmergenciasAPI.
// Su forma es la de bff.MensajeriaPort sin el bearer: el adaptador de core lo
// descarta, porque la autorización sale de los claims del ctx que pone el
// middleware del bff (auth.ClaimsFromContext), igual que en los handlers HTTP.
//
// Paridad con HTTP: cada método repite el orden del handler correspondiente
// (claims → guarda de autorización → caso de uso → traducción de error) sobre
// las MISMAS instancias de casos de uso (handlersConversacion/handlersBandeja).
// La concurrencia optimista, la idempotencia por client_message_id y la ventana
// de escritura viven en el dominio, así que llegan igual por las dos vías.
//
// El id de conversación es el que la ruta HTTP recibe en {id}
// (mensajeria.conversacion.id); esta superficie no lo reinterpreta.
//
// MarcarAtendida, los recibos y ListarAuditoriaConversacion (E3-T6) traducen sus
// errores con convhttp.TraducirAtencion, la MISMA función que usan sus handlers.
type ConversacionesAPI interface {
	ObtenerConversacion(ctx context.Context, conversacionID msgclient.Id) (*msgclient.Conversacion, error)
	ObtenerConversacionDeViaje(ctx context.Context, viajeID msgclient.Id) (*msgclient.Conversacion, error)
	ListarMensajes(ctx context.Context, conversacionID msgclient.Id, params msgclient.ListarMensajesParams) (*msgclient.PaginaMensajes, error)
	EnviarMensaje(ctx context.Context, conversacionID msgclient.Id, body msgclient.EnviarMensajeEntrada) (*msgclient.Mensaje, error)
	ListarTrabajoBandeja(ctx context.Context, params msgclient.ListarTrabajoMiBandejaParams) (*msgclient.TrabajoBandeja, error)
	ReclamarConversacion(ctx context.Context, conversacionID msgclient.Id, body msgclient.VersionEsperadaEntrada) (*msgclient.Reclamo, error)
	LiberarReclamo(ctx context.Context, conversacionID, reclamoID msgclient.Id, params msgclient.LiberarReclamoParams) error
	TransferirConversacion(ctx context.Context, conversacionID msgclient.Id, body msgclient.TransferenciaEntrada) (*msgclient.Reclamo, error)
	MarcarAtendida(ctx context.Context, conversacionID msgclient.Id, body msgclient.VersionEsperadaEntrada) error
	RegistrarReciboEntregado(ctx context.Context, conversacionID msgclient.Id, body msgclient.ReciboEntrada) error
	RegistrarReciboLeido(ctx context.Context, conversacionID msgclient.Id, body msgclient.ReciboEntrada) error
	ListarAuditoriaConversacion(ctx context.Context, conversacionID msgclient.Id, params msgclient.ListarAuditoriaConversacionParams) (*msgclient.PaginaAuditoria, error)
}

// Conversaciones construye la superficie pública de conversación y bandeja sobre
// el mismo cableado que Routes. No abre recursos.
func (m *Module) Conversaciones() ConversacionesAPI {
	return &conversacionesAPI{conv: m.handlersConversacion(), bandeja: m.handlersBandeja()}
}

// conversacionesAPI reutiliza las dependencias de los handlers HTTP (sus
// interfaces de consumidor), no un juego propio.
//
// shortcut: el flujo de cada handler (guarda, mapeo de errores) está repetido
// aquí, como en EmergenciasAPI: los handlers viven en un paquete que el root
// importa y no pueden llamar al root sin ciclo. Upgrade: si un mapeo de error
// cambia en un handler, cambiarlo aquí en el mismo PR (la prueba de esta
// superficie fija las categorías).
type conversacionesAPI struct {
	conv    convhttp.Handlers
	bandeja bandejashttp.Handlers
}

func (a *conversacionesAPI) ObtenerConversacion(ctx context.Context, conversacionID msgclient.Id) (*msgclient.Conversacion, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	id := conversacionID.String()
	tenant, err := a.conv.Autz.Conversacion(ctx, id, claims, autorizacion.Leer)
	if err != nil {
		return nil, err
	}
	conv, err := a.conv.Conversacion.ObtenerConversacion(ctx, tenant, id)
	if errors.Is(err, conversacion.ErrConversacionNoEncontrada) {
		return nil, platformerrors.NewNoEncontrado("conversacion no encontrada")
	}
	if err != nil {
		return nil, traducir(err)
	}
	return conv, nil
}

// ObtenerConversacionDeViaje recorre el MISMO camino que el handler
// (convhttp.Handlers.ObtenerDeViaje): resolver por viaje → acceso → lectura.
func (a *conversacionesAPI) ObtenerConversacionDeViaje(ctx context.Context, viajeID msgclient.Id) (*msgclient.Conversacion, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	return a.conv.ObtenerDeViaje(ctx, claims, viajeID.String())
}

func (a *conversacionesAPI) ListarMensajes(ctx context.Context, conversacionID msgclient.Id, params msgclient.ListarMensajesParams) (*msgclient.PaginaMensajes, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	id := conversacionID.String()
	tenant, err := a.conv.Autz.Conversacion(ctx, id, claims, autorizacion.Leer)
	if err != nil {
		return nil, err
	}
	if params.AntesDe != nil && params.DespuesDe != nil {
		return nil, platformerrors.NewValidacion("antes_de y despues_de son mutuamente excluyentes")
	}
	lim := 50
	if params.Limite != nil {
		lim = *params.Limite
	}
	items, hayMas, err := a.conv.Conversacion.ListarMensajes(ctx, tenant, id, params.AntesDe, params.DespuesDe, lim)
	if err != nil {
		return nil, traducir(err)
	}
	return &msgclient.PaginaMensajes{Items: items, HayMas: hayMas}, nil
}

func (a *conversacionesAPI) EnviarMensaje(ctx context.Context, conversacionID msgclient.Id, body msgclient.EnviarMensajeEntrada) (*msgclient.Mensaje, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	id := conversacionID.String()
	tenant, err := a.conv.Autz.Conversacion(ctx, id, claims, autorizacion.Escribir)
	if err != nil {
		return nil, err
	}
	alias, err := a.conv.ResolverAlias(ctx, tenant, id)
	if err != nil {
		return nil, traducir(err)
	}
	motivoOverride := ""
	if body.MotivoOverride != nil {
		motivoOverride = *body.MotivoOverride
	}
	res, err := a.conv.Conversacion.Enviar(ctx, conversacion.EntradaEnvio{
		TenantID:               tenant,
		ConversacionID:         id,
		ActorReal:              claims.Subject,
		ClientMessageID:        body.ClientMessageId.String(),
		Cuerpo:                 body.Cuerpo,
		AliasMostrado:          alias,
		RolAlEnviar:            claims.Rol,
		VersionReclamoEsperada: body.VersionReclamoEsperada,
		MotivoOverride:         motivoOverride,
	})
	switch {
	case errors.Is(err, conversacion.ErrConversacionSoloLectura):
		return nil, platformerrors.NewConflictoCodigo("conversacion_solo_lectura", "mensajeria: conversacion_solo_lectura")
	case errors.Is(err, conversacion.ErrSinReclamoActivo):
		return nil, platformerrors.NewConflictoCodigo("conflicto_version_reclamo", "mensajeria: conflicto_version_reclamo")
	case errors.Is(err, conversacion.ErrOverrideNoImplementado):
		return nil, platformerrors.NewValidacion("motivo_override no está disponible en V1")
	case err != nil:
		return nil, traducir(err)
	}
	mensaje, err := a.conv.Conversacion.MensajePorID(ctx, tenant, res.MensajeID)
	if err != nil {
		return nil, traducir(err)
	}
	return mensaje, nil
}

// ListarTrabajoBandeja es la ruta sin {id} (listarTrabajoMiBandeja): la unión de
// las bandejas de chat de todos los tenants (DEC-127, DEC-129), para quien la lee
// por su rol (DEC-131). Cursor se ignora, igual que en HTTP.
func (a *conversacionesAPI) ListarTrabajoBandeja(ctx context.Context, params msgclient.ListarTrabajoMiBandejaParams) (*msgclient.TrabajoBandeja, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	if claims.Audiencia == auth.AudienciaCliente || !autorizacion.LeeBandejaChat(claims.Rol) {
		return nil, platformerrors.NewNoEncontrado(bandejashttp.SinBandejaChat)
	}

	var (
		items []msgclient.Conversacion
		rev   int64
		fuera bool
	)
	if params.DesdeRevision == nil {
		items, rev, err = a.bandeja.Bandeja.TrabajoChat(ctx)
	} else {
		items, rev, fuera, err = a.bandeja.Bandeja.CambiosChatDesde(ctx, *params.DesdeRevision)
	}
	if err != nil {
		return nil, traducir(err)
	}
	if fuera {
		return nil, platformerrors.NewConflictoCodigo("stream_resync_required", "mensajeria: stream_resync_required")
	}
	if items == nil {
		items = []msgclient.Conversacion{}
	}
	return &msgclient.TrabajoBandeja{Items: items, Revision: rev}, nil
}

func (a *conversacionesAPI) ReclamarConversacion(ctx context.Context, conversacionID msgclient.Id, body msgclient.VersionEsperadaEntrada) (*msgclient.Reclamo, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	id := conversacionID.String()
	tenant, err := a.conv.Autz.Conversacion(ctx, id, claims, autorizacion.Escribir)
	if err != nil {
		return nil, err
	}
	if _, err := a.conv.Reclamos.Reclamar(ctx, tenant, id, claims.Subject, msgtipos.VersionReclamo(body.VersionEsperada)); err != nil {
		return nil, convhttp.TraducirReclamo(err)
	}
	// Misma hidratación en lectura aparte que el handler (ver su shortcut).
	reclamo, err := a.conv.Reclamo.ReclamoActivo(ctx, tenant, id)
	if err != nil {
		return nil, traducir(err)
	}
	return reclamo, nil
}

func (a *conversacionesAPI) LiberarReclamo(ctx context.Context, conversacionID, reclamoID msgclient.Id, params msgclient.LiberarReclamoParams) error {
	claims, err := claimsDe(ctx)
	if err != nil {
		return err
	}
	id := conversacionID.String()
	tenant, err := a.conv.Autz.Conversacion(ctx, id, claims, autorizacion.Escribir)
	if err != nil {
		return err
	}
	if err := a.conv.Reclamos.Liberar(ctx, tenant, id, reclamoID.String(), claims.Subject, msgtipos.VersionReclamo(params.VersionEsperada)); err != nil {
		return convhttp.TraducirReclamo(err)
	}
	return nil
}

func (a *conversacionesAPI) TransferirConversacion(ctx context.Context, conversacionID msgclient.Id, body msgclient.TransferenciaEntrada) (*msgclient.Reclamo, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	id := conversacionID.String()
	tenant, err := a.conv.Autz.Conversacion(ctx, id, claims, autorizacion.Escribir)
	if err != nil {
		return nil, err
	}
	motivo := ""
	if body.Motivo != nil {
		motivo = *body.Motivo
	}
	err = a.conv.Reclamos.Transferir(ctx, tenant, id, claims.Subject,
		body.ResponsableDestinoId.String(), motivo, msgtipos.VersionReclamo(body.VersionEsperada))
	if err != nil {
		return nil, convhttp.TraducirReclamo(err)
	}
	reclamo, err := a.conv.Reclamo.ReclamoActivo(ctx, tenant, id)
	if err != nil {
		return nil, traducir(err)
	}
	return reclamo, nil
}

// MarcarAtendida: mismo orden que el handler (claims → acceso → caso de uso).
func (a *conversacionesAPI) MarcarAtendida(ctx context.Context, conversacionID msgclient.Id, body msgclient.VersionEsperadaEntrada) error {
	claims, err := claimsDe(ctx)
	if err != nil {
		return err
	}
	id := conversacionID.String()
	tenant, err := a.conv.Autz.Conversacion(ctx, id, claims, autorizacion.Escribir)
	if err != nil {
		return err
	}
	if err := a.conv.Atencion.MarcarAtendida(ctx, tenant, id, claims.Subject, msgtipos.VersionReclamo(body.VersionEsperada)); err != nil {
		return convhttp.TraducirAtencion(err)
	}
	return nil
}

func (a *conversacionesAPI) RegistrarReciboEntregado(ctx context.Context, conversacionID msgclient.Id, body msgclient.ReciboEntrada) error {
	return a.registrarRecibo(ctx, conversacionID, conversacion.ReciboEntregado, body)
}

func (a *conversacionesAPI) RegistrarReciboLeido(ctx context.Context, conversacionID msgclient.Id, body msgclient.ReciboEntrada) error {
	return a.registrarRecibo(ctx, conversacionID, conversacion.ReciboLeido, body)
}

func (a *conversacionesAPI) registrarRecibo(ctx context.Context, conversacionID msgclient.Id, tipo conversacion.Recibo, body msgclient.ReciboEntrada) error {
	claims, err := claimsDe(ctx)
	if err != nil {
		return err
	}
	id := conversacionID.String()
	tenant, err := a.conv.Autz.Conversacion(ctx, id, claims, autorizacion.Escribir)
	if err != nil {
		return err
	}
	if err := a.conv.Atencion.RegistrarRecibo(ctx, tenant, id, claims.Subject, tipo, msgtipos.SecuenciaMensaje(body.HastaSecuencia)); err != nil {
		return convhttp.TraducirAtencion(err)
	}
	return nil
}

// ListarAuditoriaConversacion: solo administrador (DEC-036), verificado antes de
// leer, igual que el handler.
func (a *conversacionesAPI) ListarAuditoriaConversacion(ctx context.Context, conversacionID msgclient.Id, params msgclient.ListarAuditoriaConversacionParams) (*msgclient.PaginaAuditoria, error) {
	claims, err := claimsDe(ctx)
	if err != nil {
		return nil, err
	}
	if claims.Rol != convhttp.RolAdministrador {
		return nil, platformerrors.NewProhibido("solo administrador lee la auditoría")
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	tenant, err := a.conv.Autz.TenantDeConversacion(ctx, conversacionID.String(), claims)
	if err != nil {
		return nil, err
	}
	pagina, err := a.conv.Auditoria.Listar(ctx, tenant, conversacionID.String(), cursor)
	if err != nil {
		return nil, convhttp.TraducirAtencion(err)
	}
	return pagina, nil
}
