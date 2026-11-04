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

// TestConversacionesAPIAtencion llama MarcarAtendida, los recibos y
// ListarAuditoriaConversacion como los llamará el adaptador en proceso de
// pf-svc-core (claims en el ctx, sin bearer) y compara con el status del handler
// HTTP sobre el mismo módulo donde importa la paridad.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestConversacionesAPIAtencion(t *testing.T) {
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
		cliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		bandeja = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		ejec    = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		otro    = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
		adminID = "f0f0f0f0-f0f0-f0f0-f0f0-f0f0f0f0f0f0"
	)
	// Conversación con 2 mensajes (siguiente_secuencia = 2) y reclamo de ejec en v1.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+conv+`', '`+tenant+`', gen_random_uuid(), '`+cliente+`', 'abierta', 2)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre) VALUES ('`+bandeja+`', '`+tenant+`', 'chat', 'cliente A')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa)
		 VALUES ('`+conv+`', '`+tenant+`', '`+bandeja+`', true)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version)
		 VALUES ('`+conv+`', '`+tenant+`', '`+ejec+`', 1)`)

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

	como := func(sub, rol string) context.Context {
		// El interno llega con el tenant de AI27, no el del cliente (DEC-129).
		return auth.ContextWithClaims(ctx, &auth.Claims{Subject: sub, TenantID: "ai27", Rol: rol, Audiencia: auth.AudienciaInterno})
	}
	categoria := func(err error) platformerrors.Category { return platformerrors.AsCategory(err) }
	statusHTTP := func(ctxReq context.Context, metodo, ruta string, cuerpo any) int {
		b, _ := json.Marshal(cuerpo)
		req := httptest.NewRequest(metodo, ruta, bytes.NewReader(b)).WithContext(ctxReq)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	leidoAI27 := func() int64 {
		var n int64
		if err := admin.QueryRow(ctx, `SELECT leido_ai27_hasta FROM mensajeria.conversacion WHERE id = $1`, conv).Scan(&n); err != nil {
			t.Fatalf("leer leido_ai27_hasta: %v", err)
		}
		return n
	}
	convUUID := uuid.MustParse(conv)
	rutaAtendida := "/api/v1/conversaciones/" + conv + "/atendida"

	// Atendida: no responsable → prohibido; versión obsoleta → conflicto;
	// ambas con el mismo status que HTTP. El responsable en v1 → ok.
	err = api.MarcarAtendida(como(otro, "ejecutivo"), convUUID, msgclient.VersionEsperadaEntrada{VersionEsperada: 1})
	if categoria(err) != platformerrors.CategoryProhibido {
		t.Fatalf("atendida por no responsable: categoría %v, quiere prohibido (err %v)", categoria(err), err)
	}
	if got := statusHTTP(como(otro, "ejecutivo"), http.MethodPost, rutaAtendida, msgclient.VersionEsperadaEntrada{VersionEsperada: 1}); got != categoria(err).HTTPStatus() {
		t.Fatalf("paridad no responsable: HTTP %d, en proceso %d", got, categoria(err).HTTPStatus())
	}
	err = api.MarcarAtendida(como(ejec, "ejecutivo"), convUUID, msgclient.VersionEsperadaEntrada{VersionEsperada: 0})
	if categoria(err) != platformerrors.CategoryConflicto {
		t.Fatalf("atendida con versión obsoleta: categoría %v, quiere conflicto", categoria(err))
	}
	if got := statusHTTP(como(ejec, "ejecutivo"), http.MethodPost, rutaAtendida, msgclient.VersionEsperadaEntrada{VersionEsperada: 0}); got != http.StatusConflict {
		t.Fatalf("paridad versión obsoleta: HTTP %d, quiere 409", got)
	}
	if err := api.MarcarAtendida(como(ejec, "ejecutivo"), convUUID, msgclient.VersionEsperadaEntrada{VersionEsperada: 1}); err != nil {
		t.Fatalf("atendida por el responsable: %v", err)
	}
	c, err := api.ObtenerConversacion(como(ejec, "ejecutivo"), convUUID)
	if err != nil || c.AtendidaEn == nil {
		t.Fatalf("tras atendida: err %v, atendida_en %v; quiere presente", err, c)
	}

	// Recibos: el de un no responsable no mueve el leído visible; el del
	// responsable sí, recortado a la última secuencia (2).
	if err := api.RegistrarReciboLeido(como(otro, "ejecutivo"), convUUID, msgclient.ReciboEntrada{HastaSecuencia: 2}); err != nil {
		t.Fatalf("recibo leído de otro: %v", err)
	}
	if got := leidoAI27(); got != 0 {
		t.Fatalf("leído AI27 tras vista incidental: %d, quiere 0", got)
	}
	if err := api.RegistrarReciboEntregado(como(ejec, "ejecutivo"), convUUID, msgclient.ReciboEntrada{HastaSecuencia: 2}); err != nil {
		t.Fatalf("recibo entregado del responsable: %v", err)
	}
	if got := leidoAI27(); got != 0 {
		t.Fatalf("leído AI27 tras entregado: %d, quiere 0", got)
	}
	if err := api.RegistrarReciboLeido(como(ejec, "ejecutivo"), convUUID, msgclient.ReciboEntrada{HastaSecuencia: 5}); err != nil {
		t.Fatalf("recibo leído del responsable: %v", err)
	}
	if got := leidoAI27(); got != 2 {
		t.Fatalf("leído AI27 tras recibo del responsable: %d, quiere 2", got)
	}
	// Un rol que no es miembro de la bandeja (DEC-127) recibe 403 (DEC-132).
	if err := api.RegistrarReciboLeido(como(uuid.NewString(), "monitorista"), convUUID, msgclient.ReciboEntrada{HastaSecuencia: 1}); categoria(err) != platformerrors.CategoryProhibido {
		t.Fatalf("recibo de monitorista: categoría %v, quiere prohibido", categoria(err))
	}
	// Un supervisor lee pero no registra recibos (DEC-131).
	if err := api.RegistrarReciboLeido(como(uuid.NewString(), "supervisor"), convUUID, msgclient.ReciboEntrada{HastaSecuencia: 1}); categoria(err) != platformerrors.CategoryProhibido {
		t.Fatalf("recibo de supervisor: categoría %v, quiere prohibido", categoria(err))
	}

	// Auditoría: solo administrador; trae la atendida con su actor.
	if _, err := api.ListarAuditoriaConversacion(como(ejec, "ejecutivo"), convUUID, msgclient.ListarAuditoriaConversacionParams{}); categoria(err) != platformerrors.CategoryProhibido {
		t.Fatalf("auditoría como ejecutivo: categoría %v, quiere prohibido", categoria(err))
	}
	if got := statusHTTP(como(ejec, "ejecutivo"), http.MethodGet, "/api/v1/auditoria/conversaciones/"+conv, nil); got != http.StatusForbidden {
		t.Fatalf("paridad auditoría como ejecutivo: HTTP %d, quiere 403", got)
	}
	pag, err := api.ListarAuditoriaConversacion(como(adminID, "administrador"), convUUID, msgclient.ListarAuditoriaConversacionParams{})
	if err != nil {
		t.Fatalf("auditoría como admin: %v", err)
	}
	if len(pag.Items) != 1 || pag.Items[0].Accion != "conversacion.atendida" || pag.Items[0].ActorReal.String() != ejec {
		t.Fatalf("auditoría: %+v; quiere solo conversacion.atendida de %s", pag.Items, ejec)
	}
	malo := "no-es-cursor"
	if _, err := api.ListarAuditoriaConversacion(como(adminID, "administrador"), convUUID, msgclient.ListarAuditoriaConversacionParams{Cursor: &malo}); categoria(err) != platformerrors.CategoryValidacion {
		t.Fatalf("cursor inválido: categoría %v, quiere validacion", categoria(err))
	}
	if _, err := api.ListarAuditoriaConversacion(como(adminID, "administrador"), uuid.New(), msgclient.ListarAuditoriaConversacionParams{}); categoria(err) != platformerrors.CategoryNoEncontrado {
		t.Fatalf("auditoría de conversación inexistente: categoría %v, quiere no_encontrado", categoria(err))
	}

	// Paginación: con 51 filas, la primera página trae 50 y un cursor; la segunda,
	// la fila restante y cursor null.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.auditoria_mensajeria (tenant_id, conversacion_id, actor_real, accion)
		 SELECT '`+tenant+`', '`+conv+`', '`+otro+`', 'relleno' FROM generate_series(1, 50)`)
	p1, err := api.ListarAuditoriaConversacion(como(adminID, "administrador"), convUUID, msgclient.ListarAuditoriaConversacionParams{})
	if err != nil || len(p1.Items) != 50 || p1.CursorSiguiente == nil {
		t.Fatalf("página 1: err %v, %d items, cursor %v; quiere 50 y cursor", err, len(p1.Items), p1.CursorSiguiente)
	}
	p2, err := api.ListarAuditoriaConversacion(como(adminID, "administrador"), convUUID, msgclient.ListarAuditoriaConversacionParams{Cursor: p1.CursorSiguiente})
	if err != nil || len(p2.Items) != 1 || p2.CursorSiguiente != nil || p2.Items[0].Accion != "relleno" {
		t.Fatalf("página 2: err %v, %+v; quiere 1 fila 'relleno' y cursor null", err, p2)
	}
}
