package conversacion_test

import (
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
)

// Handlers de lectura de la conversación del viaje (E9-T8 / Ola B.0), de punta a
// punta contra Postgres real. Confirman el aislamiento del cliente (dueño ve su
// conversación; otro cliente recibe 404) y la hidratación de la vista. Se saltan
// si MENSAJERIA_TEST_DB no está definido.
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
		tenant  = "a1000000-0000-0000-0000-000000000001" // tenant = cliente (DEC-049)
		conv    = "11111111-1111-1111-1111-111111111111"
		viaje   = "bbbbbbbb-1111-1111-1111-111111111111"
		actor   = "22222222-2222-2222-2222-222222222222"
		otroCli = "a9000000-0000-0000-0000-000000000009"
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

	// El cliente dueño: su sub (usuario-cliente) es el cliente_id; para audiencia
	// Cliente, autorizacion compara cliente_id == sub.
	cliente := func(sub string) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: tenant, Rol: "cliente", Audiencia: auth.AudienciaCliente}
	}
	get := func(path string, c *auth.Claims) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.ContextWithClaims(ctx, c))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// obtenerConversacion — el dueño ve su conversación (200) con la vista hidratada.
	rec := get("/api/v1/conversaciones/"+conv, cliente(tenant))
	if rec.Code != http.StatusOK {
		t.Fatalf("obtener (dueño): status %d, quiere 200; cuerpo %s", rec.Code, rec.Body.String())
	}
	var convResp msgclient.Conversacion
	if err := json.Unmarshal(rec.Body.Bytes(), &convResp); err != nil {
		t.Fatalf("decodificar conversacion: %v", err)
	}
	if convResp.UltimaSecuencia != 1 {
		t.Errorf("ultima_secuencia: got %d, want 1", convResp.UltimaSecuencia)
	}

	// Aislamiento: OTRO cliente no es dueño. mensajeria responde 401 (criterio
	// preexistente de autorizacion.clientePuede: 404 solo si la conversación no
	// existe; 401 si existe pero no eres el dueño). Nota: difiere del 404 que usa
	// operacion para viaje ajeno (E9-T7/DEC-111); unificar el criterio entre
	// superficies de cliente es una decisión aparte, no de esta ola.
	if rec := get("/api/v1/conversaciones/"+conv, cliente(otroCli)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("obtener (ajeno): status %d, quiere 401; cuerpo %s", rec.Code, rec.Body.String())
	}

	// listarMensajes — el dueño ve la historia (200) con el mensaje sembrado.
	rec = get("/api/v1/conversaciones/"+conv+"/mensajes", cliente(tenant))
	if rec.Code != http.StatusOK {
		t.Fatalf("listar (dueño): status %d, quiere 200; cuerpo %s", rec.Code, rec.Body.String())
	}
	var pagina msgclient.PaginaMensajes
	if err := json.Unmarshal(rec.Body.Bytes(), &pagina); err != nil {
		t.Fatalf("decodificar pagina: %v", err)
	}
	if len(pagina.Items) != 1 || pagina.Items[0].Cuerpo != "hola" {
		t.Errorf("mensajes: esperaba 1 con cuerpo 'hola', got %d", len(pagina.Items))
	}

	// Aislamiento en la lista: otro cliente ⇒ 401 (mismo criterio que arriba).
	if rec := get("/api/v1/conversaciones/"+conv+"/mensajes", cliente(otroCli)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("listar (ajeno): status %d, quiere 401; cuerpo %s", rec.Code, rec.Body.String())
	}
}
