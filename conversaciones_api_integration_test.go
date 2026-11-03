package mensajeria_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	platformpgx "github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/google/uuid"
)

// TestConversacionesAPI llama a la superficie en proceso como la llamará el
// adaptador de pf-svc-core (claims en el ctx, sin bearer) contra Postgres real y
// afirma lo que observa el consumidor. Donde importa la paridad (denegación y
// conflicto de versión), compara además con el status del handler HTTP sobre el
// mismo módulo.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestConversacionesAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tenant  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" // tenant = cliente (DEC-112)
		conv    = "11111111-1111-1111-1111-111111111111"
		convSL  = "22222222-2222-2222-2222-222222222222" // solo_lectura
		cliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		bandeja = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		ejec    = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		destino = "ffffffff-ffff-ffff-ffff-ffffffffffff"
		otro    = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
		msgID   = "99999999-9999-9999-9999-999999999999"
	)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+conv+`', '`+tenant+`', gen_random_uuid(), '`+cliente+`', 'abierta', 0),
		        ('`+convSL+`', '`+tenant+`', gen_random_uuid(), '`+cliente+`', 'solo_lectura', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre, revision)
		 VALUES ('`+bandeja+`', '`+tenant+`', 'chat', 'cliente A', 1)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa, revision)
		 VALUES ('`+conv+`', '`+tenant+`', '`+bandeja+`', true, 1)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{})
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	api := mod.Conversaciones()
	mux := http.NewServeMux()
	mod.Routes(mux)

	como := func(sub, rol string, aud auth.Audiencia) context.Context {
		return auth.ContextWithClaims(ctx, &auth.Claims{Subject: sub, TenantID: tenant, Rol: rol, Audiencia: aud})
	}
	interno := func(sub string) context.Context { return como(sub, "ejecutivo", auth.AudienciaInterno) }
	ctxCliente := como(cliente, "cliente", auth.AudienciaCliente)
	categoria := func(err error) platformerrors.Category { return platformerrors.AsCategory(err) }
	// statusHTTP ejecuta la misma operación por el handler HTTP con los mismos claims.
	statusHTTP := func(ctxReq context.Context, metodo, ruta string, cuerpo any) int {
		b, _ := json.Marshal(cuerpo)
		req := httptest.NewRequest(metodo, ruta, bytes.NewReader(b)).WithContext(ctxReq)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	convUUID := uuid.MustParse(conv)

	// Sin claims: no autorizado.
	if _, err := api.ObtenerConversacion(context.Background(), convUUID); categoria(err) != platformerrors.CategoryNoAutorizado {
		t.Fatalf("sin claims: categoría %v, quiere no_autorizado (err %v)", categoria(err), err)
	}

	// Bandeja del sujeto (sin {id}): bootstrap y catch-up antes de mutar nada.
	tb, err := api.ListarTrabajoBandeja(interno(ejec), msgclient.ListarTrabajoMiBandejaParams{})
	if err != nil {
		t.Fatalf("ListarTrabajoBandeja: %v", err)
	}
	if tb.Revision != 1 || len(tb.Items) != 1 || tb.Items[0].Id.String() != conv {
		t.Fatalf("bootstrap: revision %d, %d items; quiere revision 1 y solo %s", tb.Revision, len(tb.Items), conv)
	}
	// Catch-up desde la revisión vigente: nada cambió después de ella.
	desde := int64(1)
	tb2, err := api.ListarTrabajoBandeja(interno(ejec), msgclient.ListarTrabajoMiBandejaParams{DesdeRevision: &desde})
	if err != nil {
		t.Fatalf("catch-up desde 1: %v", err)
	}
	if len(tb2.Items) != 0 || tb2.Revision != 1 {
		t.Fatalf("catch-up desde 1: %d items, revision %d; quiere 0 items y revision 1", len(tb2.Items), tb2.Revision)
	}
	// Un rol que no es Ejecutivo no tiene bandeja (DEC-127): el mismo 404 que
	// antes recibía quien no tenía membresía, por las dos vías.
	ctxMonitorista := como(otro, "monitorista", auth.AudienciaInterno)
	if _, err := api.ListarTrabajoBandeja(ctxMonitorista, msgclient.ListarTrabajoMiBandejaParams{}); categoria(err) != platformerrors.CategoryNoEncontrado {
		t.Fatalf("bandeja de monitorista: categoría %v, quiere no_encontrado", categoria(err))
	}
	if got := statusHTTP(ctxMonitorista, http.MethodGet, "/api/v1/bandeja/trabajo", nil); got != http.StatusNotFound {
		t.Fatalf("paridad bandeja de monitorista: HTTP %d, quiere 404", got)
	}

	// Obtener: el cliente dueño la lee; sin reclamo, version_reclamo 0.
	c, err := api.ObtenerConversacion(ctxCliente, convUUID)
	if err != nil {
		t.Fatalf("ObtenerConversacion: %v", err)
	}
	if c.Id.String() != conv || c.Estado != "abierta" || c.VersionReclamo != 0 {
		t.Fatalf("conversación: id %s estado %s version %d; quiere %s abierta 0", c.Id, c.Estado, c.VersionReclamo, conv)
	}

	// Denegación: un interno que no es Ejecutivo no reclama (DEC-127), con la
	// misma categoría que antes sin membresía y que HTTP 401.
	_, err = api.ReclamarConversacion(ctxMonitorista, convUUID, msgclient.VersionEsperadaEntrada{VersionEsperada: 0})
	if categoria(err) != platformerrors.CategoryNoAutorizado {
		t.Fatalf("reclamar como monitorista: categoría %v, quiere no_autorizado", categoria(err))
	}
	rutaReclamos := "/api/v1/conversaciones/" + conv + "/reclamos"
	if got := statusHTTP(ctxMonitorista, http.MethodPost, rutaReclamos, msgclient.VersionEsperadaEntrada{}); got != categoria(err).HTTPStatus() {
		t.Fatalf("paridad denegación: HTTP %d, en proceso %d", got, categoria(err).HTTPStatus())
	}

	// Reclamo: versión 0 → reclamo v1 del ejecutivo.
	r, err := api.ReclamarConversacion(interno(ejec), convUUID, msgclient.VersionEsperadaEntrada{VersionEsperada: 0})
	if err != nil {
		t.Fatalf("ReclamarConversacion: %v", err)
	}
	if r.Version != 1 || r.ResponsableId.String() != ejec {
		t.Fatalf("reclamo: version %d responsable %s; quiere 1 %s", r.Version, r.ResponsableId, ejec)
	}
	// Conflicto de versión: reclamar otra vez con la versión vieja.
	_, err = api.ReclamarConversacion(interno(ejec), convUUID, msgclient.VersionEsperadaEntrada{VersionEsperada: 0})
	if categoria(err) != platformerrors.CategoryConflicto {
		t.Fatalf("reclamo con versión vieja: categoría %v, quiere conflicto", categoria(err))
	}
	if got := statusHTTP(interno(ejec), http.MethodPost, rutaReclamos, msgclient.VersionEsperadaEntrada{}); got != http.StatusConflict {
		t.Fatalf("paridad conflicto: HTTP %d, quiere 409", got)
	}

	// Envío interno con el reclamo vigente; el reintento con el mismo
	// client_message_id devuelve el mismo mensaje (idempotencia).
	v1 := msgclient.VersionReclamo(1)
	envio := msgclient.EnviarMensajeEntrada{ClientMessageId: uuid.MustParse(msgID), Cuerpo: "hola", VersionReclamoEsperada: &v1}
	m1, err := api.EnviarMensaje(interno(ejec), convUUID, envio)
	if err != nil {
		t.Fatalf("EnviarMensaje: %v", err)
	}
	if m1.Cuerpo != "hola" || m1.Secuencia != 1 {
		t.Fatalf("mensaje: cuerpo %q secuencia %d; quiere hola 1", m1.Cuerpo, m1.Secuencia)
	}
	m2, err := api.EnviarMensaje(interno(ejec), convUUID, envio)
	if err != nil || m2.Id != m1.Id {
		t.Fatalf("reintento: err %v, id %v; quiere el mismo id %v", err, m2, m1.Id)
	}
	// Ventana de escritura: una conversación solo_lectura rechaza el envío.
	_, err = api.EnviarMensaje(ctxCliente, uuid.MustParse(convSL), msgclient.EnviarMensajeEntrada{ClientMessageId: uuid.New(), Cuerpo: "tarde"})
	if categoria(err) != platformerrors.CategoryConflicto {
		t.Fatalf("envío a solo_lectura: categoría %v, quiere conflicto", categoria(err))
	}

	// Historia: el único mensaje, sin más páginas; cursores excluyentes → validación.
	pag, err := api.ListarMensajes(ctxCliente, convUUID, msgclient.ListarMensajesParams{})
	if err != nil {
		t.Fatalf("ListarMensajes: %v", err)
	}
	if len(pag.Items) != 1 || pag.Items[0].Id != m1.Id || pag.HayMas {
		t.Fatalf("historia: %d items, hay_mas %v; quiere 1 (%v) y false", len(pag.Items), pag.HayMas, m1.Id)
	}
	uno := int64(1)
	if _, err := api.ListarMensajes(ctxCliente, convUUID, msgclient.ListarMensajesParams{AntesDe: &uno, DespuesDe: &uno}); categoria(err) != platformerrors.CategoryValidacion {
		t.Fatalf("antes_de+despues_de: categoría %v, quiere validacion", categoria(err))
	}

	// Transferir al otro miembro: el reclamo nuevo es del destino, en la versión
	// siguiente (2).
	rt, err := api.TransferirConversacion(interno(ejec), convUUID, msgclient.TransferenciaEntrada{ResponsableDestinoId: uuid.MustParse(destino), VersionEsperada: 1})
	if err != nil {
		t.Fatalf("TransferirConversacion: %v", err)
	}
	if rt.ResponsableId.String() != destino || rt.Version != 2 {
		t.Fatalf("transferencia: responsable %s version %d; quiere %s 2", rt.ResponsableId, rt.Version, destino)
	}
	// Solo el responsable libera (DEC-118): el origen, ya sin reclamo, recibe
	// prohibido con la versión vigente, igual que por HTTP.
	err = api.LiberarReclamo(interno(ejec), convUUID, rt.Id, msgclient.LiberarReclamoParams{VersionEsperada: 2})
	if categoria(err) != platformerrors.CategoryProhibido {
		t.Fatalf("liberar ajeno: categoría %v, quiere prohibido", categoria(err))
	}
	rutaLiberar := "/api/v1/conversaciones/" + conv + "/reclamos/" + rt.Id.String() + "?version_esperada=2"
	if got := statusHTTP(interno(ejec), http.MethodDelete, rutaLiberar, nil); got != http.StatusForbidden {
		t.Fatalf("paridad liberar ajeno: HTTP %d, quiere 403", got)
	}

	// Liberar: el destino suelta su reclamo; liberar de nuevo ya no encuentra reclamo.
	if err := api.LiberarReclamo(interno(destino), convUUID, rt.Id, msgclient.LiberarReclamoParams{VersionEsperada: 2}); err != nil {
		t.Fatalf("LiberarReclamo: %v", err)
	}
	if err := api.LiberarReclamo(interno(destino), convUUID, rt.Id, msgclient.LiberarReclamoParams{VersionEsperada: 2}); categoria(err) != platformerrors.CategoryNoEncontrado {
		t.Fatalf("liberar sin reclamo: categoría %v, quiere no_encontrado", categoria(err))
	}
}
