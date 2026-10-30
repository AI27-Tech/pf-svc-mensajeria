package conversacion_test

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
	platformpgx "github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Identificadores compartidos por las pruebas de lectura/atendida.
const (
	ltTenant  = "t1"
	ltConv    = "11111111-1111-1111-1111-111111111111"
	ltCliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	ltBandeja = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	ltEjec    = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	ltOtro    = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
)

// cablearModulo construye el módulo sobre el pool de pruebas y devuelve su mux.
func cablearModulo(t *testing.T, pool *pgxpool.Pool) *http.ServeMux {
	t.Helper()
	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{})
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	mux := http.NewServeMux()
	mod.Routes(mux)
	return mux
}

// TestListarMensajesHTTP prueba la lectura de la historia por secuencia de
// dominio: dos mensajes, uno del cliente y uno del ejecutivo, devueltos en orden
// ascendente de secuencia con el audiencia_autor correcto (el "de_cliente":
// Cliente para el mensaje del cliente, Interno para el del ejecutivo). Además, la
// proyección Cliente no expone actor_real ni rol_al_enviar.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestListarMensajesHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+ltConv+`', '`+ltTenant+`', gen_random_uuid(), '`+ltCliente+`', 'abierta', 2)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre)
		 VALUES ('`+ltBandeja+`', '`+ltTenant+`', 'chat', 'cliente A')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa)
		 VALUES ('`+ltConv+`', '`+ltTenant+`', '`+ltBandeja+`', true)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, version, activa)
		 VALUES ('`+ltBandeja+`', '`+ltEjec+`', '`+ltTenant+`', 'ejecutivo', 1, true)`)
	// Mensaje del cliente (secuencia 1, sin rol → audiencia Cliente).
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.mensaje (conversacion_id, tenant_id, secuencia, actor_real, client_message_id, cuerpo)
		 VALUES ('`+ltConv+`', '`+ltTenant+`', 1, '`+ltCliente+`', gen_random_uuid(), 'hola, ¿dónde va mi carga?')`)
	// Mensaje del ejecutivo (secuencia 2, rol ejecutivo → audiencia Interno).
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.mensaje (conversacion_id, tenant_id, secuencia, actor_real, rol_al_enviar, alias_mostrado, client_message_id, cuerpo)
		 VALUES ('`+ltConv+`', '`+ltTenant+`', 2, '`+ltEjec+`', 'ejecutivo', 'Tu equipo AI27', gen_random_uuid(), 'va en ruta, todo en orden')`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	mux := cablearModulo(t, pool)

	// Lectura como el CLIENTE dueño.
	cliente := &auth.Claims{Subject: ltCliente, TenantID: ltTenant, Rol: "cliente", Audiencia: auth.AudienciaCliente}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversaciones/"+ltConv+"/mensajes", nil)
	req = req.WithContext(auth.ContextWithClaims(ctx, cliente))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("listar mensajes: status %d, quiere 200; cuerpo %s", rec.Code, rec.Body.String())
	}
	var pagina msgclient.PaginaMensajes
	if err := json.Unmarshal(rec.Body.Bytes(), &pagina); err != nil {
		t.Fatalf("decodificar PaginaMensajes: %v; cuerpo %s", err, rec.Body.String())
	}
	if len(pagina.Items) != 2 {
		t.Fatalf("quiere 2 mensajes, hay %d", len(pagina.Items))
	}
	// Orden ascendente por secuencia de dominio.
	if pagina.Items[0].Secuencia != 1 || pagina.Items[1].Secuencia != 2 {
		t.Fatalf("orden incorrecto: secuencias %d, %d", pagina.Items[0].Secuencia, pagina.Items[1].Secuencia)
	}
	// de_cliente: el primero es del cliente (Cliente), el segundo del ejecutivo (Interno).
	if pagina.Items[0].AudienciaAutor != "Cliente" {
		t.Fatalf("mensaje 1 audiencia_autor = %q, quiere Cliente", pagina.Items[0].AudienciaAutor)
	}
	if pagina.Items[1].AudienciaAutor != "Interno" {
		t.Fatalf("mensaje 2 audiencia_autor = %q, quiere Interno", pagina.Items[1].AudienciaAutor)
	}
	// Proyección Cliente: no se exponen actor_real ni rol_al_enviar en NINGÚN mensaje.
	for i, m := range pagina.Items {
		if m.ActorReal != nil {
			t.Fatalf("mensaje %d expone actor_real al cliente: %v", i, *m.ActorReal)
		}
		if m.RolAlEnviar != nil {
			t.Fatalf("mensaje %d expone rol_al_enviar al cliente: %v", i, *m.RolAlEnviar)
		}
	}

	// Lectura como INTERNO (ejecutivo miembro): sí ve actor_real y rol del mensaje interno.
	interno := &auth.Claims{Subject: ltEjec, TenantID: ltTenant, Rol: "ejecutivo", Audiencia: auth.AudienciaInterno}
	reqI := httptest.NewRequest(http.MethodGet, "/api/v1/conversaciones/"+ltConv+"/mensajes", nil)
	reqI = reqI.WithContext(auth.ContextWithClaims(ctx, interno))
	recI := httptest.NewRecorder()
	mux.ServeHTTP(recI, reqI)
	if recI.Code != http.StatusOK {
		t.Fatalf("listar como interno: status %d, quiere 200; cuerpo %s", recI.Code, recI.Body.String())
	}
	var paginaI msgclient.PaginaMensajes
	if err := json.Unmarshal(recI.Body.Bytes(), &paginaI); err != nil {
		t.Fatalf("decodificar PaginaMensajes (interno): %v", err)
	}
	if paginaI.Items[1].ActorReal == nil || paginaI.Items[1].ActorReal.String() != ltEjec {
		t.Fatalf("interno debería ver actor_real del ejecutivo, got %v", paginaI.Items[1].ActorReal)
	}
	if paginaI.Items[1].RolAlEnviar == nil || string(*paginaI.Items[1].RolAlEnviar) != "ejecutivo" {
		t.Fatalf("interno debería ver rol_al_enviar del ejecutivo, got %v", paginaI.Items[1].RolAlEnviar)
	}
}

// TestObtenerConversacionProyeccionHTTP prueba la proyección por audiencia del
// detalle: el Cliente NO recibe bandeja_id ni reclamo_activo (campos internos),
// pero sí version_reclamo y el estado atendido; el Interno recibe el reclamo
// activo y la bandeja. También verifica el aislamiento por tenant: otro tenant
// no ve la conversación (404).
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestObtenerConversacionProyeccionHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+ltConv+`', '`+ltTenant+`', gen_random_uuid(), '`+ltCliente+`', 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre)
		 VALUES ('`+ltBandeja+`', '`+ltTenant+`', 'chat', 'cliente A')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa)
		 VALUES ('`+ltConv+`', '`+ltTenant+`', '`+ltBandeja+`', true)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, version, activa)
		 VALUES ('`+ltBandeja+`', '`+ltEjec+`', '`+ltTenant+`', 'ejecutivo', 1, true)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version)
		 VALUES ('`+ltConv+`', '`+ltTenant+`', '`+ltEjec+`', 1)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	mux := cablearModulo(t, pool)

	obtener := func(c *auth.Claims) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversaciones/"+ltConv, nil)
		req = req.WithContext(auth.ContextWithClaims(ctx, c))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Cliente dueño: 200, sin bandeja_id ni reclamo_activo.
	cliente := &auth.Claims{Subject: ltCliente, TenantID: ltTenant, Rol: "cliente", Audiencia: auth.AudienciaCliente}
	recC := obtener(cliente)
	if recC.Code != http.StatusOK {
		t.Fatalf("cliente obtener: status %d, quiere 200; cuerpo %s", recC.Code, recC.Body.String())
	}
	var cvC msgclient.Conversacion
	if err := json.Unmarshal(recC.Body.Bytes(), &cvC); err != nil {
		t.Fatalf("decodificar Conversacion (cliente): %v", err)
	}
	if cvC.BandejaId != nil {
		t.Fatalf("cliente no debe ver bandeja_id, got %v", *cvC.BandejaId)
	}
	if cvC.ReclamoActivo != nil {
		t.Fatalf("cliente no debe ver reclamo_activo")
	}
	if cvC.VersionReclamo != 1 {
		t.Fatalf("version_reclamo del cliente = %d, quiere 1", cvC.VersionReclamo)
	}

	// Interno miembro: 200, con bandeja_id y reclamo_activo.
	interno := &auth.Claims{Subject: ltEjec, TenantID: ltTenant, Rol: "ejecutivo", Audiencia: auth.AudienciaInterno}
	recI := obtener(interno)
	if recI.Code != http.StatusOK {
		t.Fatalf("interno obtener: status %d, quiere 200; cuerpo %s", recI.Code, recI.Body.String())
	}
	var cvI msgclient.Conversacion
	if err := json.Unmarshal(recI.Body.Bytes(), &cvI); err != nil {
		t.Fatalf("decodificar Conversacion (interno): %v", err)
	}
	if cvI.BandejaId == nil || cvI.BandejaId.String() != ltBandeja {
		t.Fatalf("interno debe ver bandeja_id %s, got %v", ltBandeja, cvI.BandejaId)
	}
	if cvI.ReclamoActivo == nil || cvI.ReclamoActivo.ResponsableId.String() != ltEjec {
		t.Fatalf("interno debe ver reclamo_activo del ejecutivo, got %v", cvI.ReclamoActivo)
	}

	// Aislamiento por tenant: otro tenant (cliente de otra organización) → 404.
	otroTenant := &auth.Claims{Subject: ltCliente, TenantID: "t2", Rol: "cliente", Audiencia: auth.AudienciaCliente}
	if rec := obtener(otroTenant); rec.Code != http.StatusNotFound {
		t.Fatalf("otro tenant: status %d, quiere 404; cuerpo %s", rec.Code, rec.Body.String())
	}

	// Aislamiento por alcance: un cliente que no es el dueño (mismo tenant) → 404,
	// no 401 (en lectura no se revela existencia).
	ajeno := &auth.Claims{Subject: ltOtro, TenantID: ltTenant, Rol: "cliente", Audiencia: auth.AudienciaCliente}
	if rec := obtener(ajeno); rec.Code != http.StatusNotFound {
		t.Fatalf("cliente ajeno: status %d, quiere 404; cuerpo %s", rec.Code, rec.Body.String())
	}
}

// TestMarcarAtendidaHTTP prueba que el responsable del reclamo activo prende el
// indicador de atendida (DEC-014), y que después obtenerConversacion lo refleja
// con atendida_en poblado. Además: un actor sin el reclamo (otro ejecutivo
// miembro) recibe 404, y una versión equivocada recibe 409.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestMarcarAtendidaHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+ltConv+`', '`+ltTenant+`', gen_random_uuid(), '`+ltCliente+`', 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre)
		 VALUES ('`+ltBandeja+`', '`+ltTenant+`', 'chat', 'cliente A')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa)
		 VALUES ('`+ltConv+`', '`+ltTenant+`', '`+ltBandeja+`', true)`)
	// Dos ejecutivos miembros; solo ltEjec tiene el reclamo activo en versión 1.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, version, activa)
		 VALUES ('`+ltBandeja+`', '`+ltEjec+`', '`+ltTenant+`', 'ejecutivo', 1, true)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, version, activa)
		 VALUES ('`+ltBandeja+`', '`+ltOtro+`', '`+ltTenant+`', 'ejecutivo', 1, true)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version)
		 VALUES ('`+ltConv+`', '`+ltTenant+`', '`+ltEjec+`', 1)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	mux := cablearModulo(t, pool)

	marcar := func(sub string, version int64) *httptest.ResponseRecorder {
		body, _ := json.Marshal(msgclient.VersionEsperadaEntrada{VersionEsperada: version})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/conversaciones/"+ltConv+"/atendida", bytes.NewReader(body))
		req = req.WithContext(auth.ContextWithClaims(ctx, &auth.Claims{Subject: sub, TenantID: ltTenant, Rol: "ejecutivo", Audiencia: auth.AudienciaInterno}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Otro ejecutivo miembro sin el reclamo: 404 (no es el responsable).
	if rec := marcar(ltOtro, 1); rec.Code != http.StatusNotFound {
		t.Fatalf("marcar sin reclamo propio: status %d, quiere 404; cuerpo %s", rec.Code, rec.Body.String())
	}

	// El responsable con versión equivocada: 409.
	if rec := marcar(ltEjec, 2); rec.Code != http.StatusConflict {
		t.Fatalf("marcar con versión equivocada: status %d, quiere 409; cuerpo %s", rec.Code, rec.Body.String())
	}

	// El responsable con la versión correcta: 204, prende el indicador.
	if rec := marcar(ltEjec, 1); rec.Code != http.StatusNoContent {
		t.Fatalf("marcar atendida: status %d, quiere 204; cuerpo %s", rec.Code, rec.Body.String())
	}

	// La fila de estado_atendido quedó encendida y atribuida al responsable.
	var atendida bool
	var atendidaPor string
	var atendidaEn *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT atendida, atendida_por::text, atendida_en FROM mensajeria.estado_atendido WHERE conversacion_id = $1`, ltConv,
	).Scan(&atendida, &atendidaPor, &atendidaEn); err != nil {
		t.Fatalf("leer estado_atendido: %v", err)
	}
	if !atendida || atendidaPor != ltEjec || atendidaEn == nil {
		t.Fatalf("estado_atendido incorrecto: atendida=%v por=%s en=%v", atendida, atendidaPor, atendidaEn)
	}

	// obtenerConversacion (Interno) ahora refleja atendida_en.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversaciones/"+ltConv, nil)
	req = req.WithContext(auth.ContextWithClaims(ctx, &auth.Claims{Subject: ltEjec, TenantID: ltTenant, Rol: "ejecutivo", Audiencia: auth.AudienciaInterno}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var cv msgclient.Conversacion
	if err := json.Unmarshal(rec.Body.Bytes(), &cv); err != nil {
		t.Fatalf("decodificar Conversacion: %v; cuerpo %s", err, rec.Body.String())
	}
	if cv.AtendidaEn == nil {
		t.Fatalf("tras marcar atendida, atendida_en debe estar poblado")
	}

	// Idempotente: volver a marcar con la misma versión deja 204.
	if rec := marcar(ltEjec, 1); rec.Code != http.StatusNoContent {
		t.Fatalf("remarcar atendida: status %d, quiere 204; cuerpo %s", rec.Code, rec.Body.String())
	}
}
