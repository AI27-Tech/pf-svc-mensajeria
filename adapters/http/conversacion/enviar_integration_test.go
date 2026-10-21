package conversacion_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	catclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformpgx "github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/google/uuid"
)

// catalogosFalso devuelve un nombre fijo por cliente, sin tocar la red. Es el
// doble del puerto CatalogosLector para probar que el alias_mostrado del mensaje
// sale del nombre del cliente de la conversación (Opción 2, DEC-097).
type catalogosFalso struct{ nombre string }

func (c catalogosFalso) ListarClientes(ctx context.Context, ids []catclient.Id) (*catclient.PaginaClientes, error) {
	items := make([]catclient.Cliente, 0, len(ids))
	for _, id := range ids {
		items = append(items, catclient.Cliente{Id: id, Nombre: c.nombre})
	}
	return &catclient.PaginaClientes{Items: items}, nil
}

// TestEnviarMensajeHTTP prueba el handler de EnviarMensaje de punta a punta
// contra Postgres real: autorización (DEC-097), envío con reclamo activo, y que
// el Mensaje devuelto trae el alias_mostrado resuelto desde catalogos (Opción 2)
// y la audiencia_autor derivada del rol. Un reintento con el mismo
// client_message_id es idempotente (mismo id, 200).
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestEnviarMensajeHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tenant  = "t1"
		conv    = "11111111-1111-1111-1111-111111111111"
		cliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		bandeja = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		ejec    = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		cmid    = "99999999-9999-9999-9999-999999999999"
	)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+conv+`', '`+tenant+`', gen_random_uuid(), '`+cliente+`', 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre)
		 VALUES ('`+bandeja+`', '`+tenant+`', 'chat', 'cliente A')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa)
		 VALUES ('`+conv+`', '`+tenant+`', '`+bandeja+`', true)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, version, activa)
		 VALUES ('`+bandeja+`', '`+ejec+`', '`+tenant+`', 'ejecutivo', 1, true)`)
	// El ejecutivo tiene el reclamo activo (enviar lo exige para audiencia Interno).
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version)
		 VALUES ('`+conv+`', '`+tenant+`', '`+ejec+`', 1)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()

	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(
		mensajeria.Pools{Chat: chat, Despacho: chat},
		mensajeria.Deps{Catalogos: catalogosFalso{nombre: "Tu equipo AI27 – Bimbo"}},
	)
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	mux := http.NewServeMux()
	mod.Routes(mux)

	claims := &auth.Claims{Subject: ejec, TenantID: tenant, Rol: "ejecutivo", Audiencia: auth.AudienciaInterno}

	enviar := func(body msgclient.EnviarMensajeEntrada) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/conversaciones/"+conv+"/mensajes", bytes.NewReader(b))
		req = req.WithContext(auth.ContextWithClaims(ctx, claims))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	ver := int64(1)
	entrada := msgclient.EnviarMensajeEntrada{
		ClientMessageId:        uuid.MustParse(cmid),
		Cuerpo:                 "hola cliente",
		VersionReclamoEsperada: &ver,
	}

	rec := enviar(entrada)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enviar: status %d, quiere 201; cuerpo %s", rec.Code, rec.Body.String())
	}
	var m msgclient.Mensaje
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decodificar Mensaje: %v; cuerpo %s", err, rec.Body.String())
	}
	if m.AliasMostrado != "Tu equipo AI27 – Bimbo" {
		t.Fatalf("alias_mostrado = %q, quiere el nombre del cliente", m.AliasMostrado)
	}
	if m.AudienciaAutor != "Interno" {
		t.Fatalf("audiencia_autor = %q, quiere Interno", m.AudienciaAutor)
	}
	if m.Secuencia != 1 {
		t.Fatalf("secuencia = %d, quiere 1", m.Secuencia)
	}

	// Reintento con el mismo client_message_id: idempotente, mismo mensaje.
	rec2 := enviar(entrada)
	if rec2.Code != http.StatusCreated && rec2.Code != http.StatusOK {
		t.Fatalf("reintento: status %d, quiere 200/201; cuerpo %s", rec2.Code, rec2.Body.String())
	}
	var m2 msgclient.Mensaje
	if err := json.Unmarshal(rec2.Body.Bytes(), &m2); err != nil {
		t.Fatalf("decodificar reintento: %v", err)
	}
	if m2.Id != m.Id {
		t.Fatalf("reintento devolvió otro mensaje: %s != %s", m2.Id, m.Id)
	}
}
