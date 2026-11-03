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
	"github.com/google/uuid"
)

// Handlers de lectura y envío de la conversación del viaje para el cliente
// (E9-T8, DEC-124), de punta a punta contra Postgres real. La conversación es de
// la empresa cliente: un usuario cuyo sub NO es el cliente_id, pero cuyo tenant
// sí lo es, la lee (200) y escribe (201); un cliente de otro tenant recibe 404.
// Se saltan si MENSAJERIA_TEST_DB no está definido.
func TestLecturaConversacionClienteHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tenant   = "a1000000-0000-0000-0000-000000000001" // tenant = cliente (DEC-112)
		conv     = "11111111-1111-1111-1111-111111111111"
		viaje    = "bbbbbbbb-1111-1111-1111-111111111111"
		actor    = "22222222-2222-2222-2222-222222222222"
		usuario  = "c1000000-0000-0000-0000-0000000000c1" // persona de la empresa: sub ≠ cliente_id
		otroCli  = "a9000000-0000-0000-0000-000000000009" // otra empresa (su tenant)
		otroUsr  = "c9000000-0000-0000-0000-0000000000c9"
		clientID = "0c000000-0000-0000-0000-000000000001"
	)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+conv+`','`+tenant+`','`+viaje+`','`+tenant+`','abierta', 1)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.mensaje (conversacion_id, tenant_id, secuencia, actor_real, client_message_id, cuerpo)
		 VALUES ('`+conv+`','`+tenant+`', 1, '`+actor+`', gen_random_uuid(), 'hola')`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()

	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{})
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	mux := http.NewServeMux()
	mod.Routes(mux)

	cliente := func(sub, tenantID string) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: tenantID, Rol: "cliente", Audiencia: auth.AudienciaCliente}
	}
	hacer := func(metodo, path string, cuerpo []byte, c *auth.Claims) *httptest.ResponseRecorder {
		req := httptest.NewRequest(metodo, path, bytes.NewReader(cuerpo))
		req = req.WithContext(auth.ContextWithClaims(ctx, c))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	get := func(path string, c *auth.Claims) *httptest.ResponseRecorder {
		return hacer(http.MethodGet, path, nil, c)
	}
	deLaEmpresa := cliente(usuario, tenant)
	deOtraEmpresa := cliente(otroUsr, otroCli)

	// obtenerConversacion: la persona de la empresa la ve (200) con la vista hidratada.
	rec := get("/api/v1/conversaciones/"+conv, deLaEmpresa)
	if rec.Code != http.StatusOK {
		t.Fatalf("obtener (empresa dueña): status %d, quiere 200; cuerpo %s", rec.Code, rec.Body.String())
	}
	var convResp msgclient.Conversacion
	if err := json.Unmarshal(rec.Body.Bytes(), &convResp); err != nil {
		t.Fatalf("decodificar conversacion: %v", err)
	}
	if convResp.UltimaSecuencia != 1 {
		t.Errorf("ultima_secuencia: got %d, want 1", convResp.UltimaSecuencia)
	}

	// Aislamiento: un cliente de otro tenant no la encuentra (404; la consulta se
	// acota por tenant).
	if rec := get("/api/v1/conversaciones/"+conv, deOtraEmpresa); rec.Code != http.StatusNotFound {
		t.Fatalf("obtener (otra empresa): status %d, quiere 404; cuerpo %s", rec.Code, rec.Body.String())
	}

	// listarMensajes: la persona de la empresa ve la historia (200).
	rec = get("/api/v1/conversaciones/"+conv+"/mensajes", deLaEmpresa)
	if rec.Code != http.StatusOK {
		t.Fatalf("listar (empresa dueña): status %d, quiere 200; cuerpo %s", rec.Code, rec.Body.String())
	}
	var pagina msgclient.PaginaMensajes
	if err := json.Unmarshal(rec.Body.Bytes(), &pagina); err != nil {
		t.Fatalf("decodificar pagina: %v", err)
	}
	if len(pagina.Items) != 1 || pagina.Items[0].Cuerpo != "hola" {
		t.Errorf("mensajes: esperaba 1 con cuerpo 'hola', got %d", len(pagina.Items))
	}
	if rec := get("/api/v1/conversaciones/"+conv+"/mensajes", deOtraEmpresa); rec.Code != http.StatusNotFound {
		t.Fatalf("listar (otra empresa): status %d, quiere 404; cuerpo %s", rec.Code, rec.Body.String())
	}

	// enviarMensaje: la persona de la empresa escribe (201, secuencia 2, autor ella).
	cuerpo, _ := json.Marshal(msgclient.EnviarMensajeEntrada{ClientMessageId: uuid.MustParse(clientID), Cuerpo: "¿ya salió?"})
	rec = hacer(http.MethodPost, "/api/v1/conversaciones/"+conv+"/mensajes", cuerpo, deLaEmpresa)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enviar (empresa dueña): status %d, quiere 201; cuerpo %s", rec.Code, rec.Body.String())
	}
	var m msgclient.Mensaje
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decodificar mensaje: %v", err)
	}
	if m.Secuencia != 2 || m.Cuerpo != "¿ya salió?" {
		t.Fatalf("mensaje enviado: secuencia %d cuerpo %q, quiere 2 y %q", m.Secuencia, m.Cuerpo, "¿ya salió?")
	}
	var autor string
	if err := admin.QueryRow(ctx, `SELECT actor_real::text FROM mensajeria.mensaje WHERE secuencia = 2 AND conversacion_id = $1`, conv).Scan(&autor); err != nil {
		t.Fatalf("leer autor: %v", err)
	}
	if autor != usuario {
		t.Fatalf("actor_real = %s, quiere la persona %s", autor, usuario)
	}
	if rec := hacer(http.MethodPost, "/api/v1/conversaciones/"+conv+"/mensajes", cuerpo, deOtraEmpresa); rec.Code != http.StatusNotFound {
		t.Fatalf("enviar (otra empresa): status %d, quiere 404; cuerpo %s", rec.Code, rec.Body.String())
	}
}
