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
)

// TestReclamarConversacionHTTP prueba el handler de ReclamarConversacion de
// punta a punta contra Postgres real: autorización (DEC-097) + el conflicto de
// versión que el dominio ya implementa (addendum §4 punto 5). Llama al handler
// como lo llama bff (con claims en contexto, como los inyecta auth.Middleware) y
// afirma el status y el cuerpo que el cliente observa.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestReclamarConversacionHTTP(t *testing.T) {
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
		otro    = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
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

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()

	// Pools espera el wrapper de plataforma, que embebe *pgxpool.Pool.
	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(
		mensajeria.Pools{Chat: chat, Despacho: chat},
		mensajeria.Deps{},
	)
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	mux := http.NewServeMux()
	mod.Routes(mux)

	interno := func(sub string) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: tenant, Rol: "ejecutivo", Audiencia: auth.AudienciaInterno}
	}

	// Llama al handler con los claims ya en contexto (como auth.Middleware).
	reclamar := func(sub string, versionEsperada int64) *httptest.ResponseRecorder {
		body, _ := json.Marshal(msgclient.VersionEsperadaEntrada{VersionEsperada: versionEsperada})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/conversaciones/"+conv+"/reclamos", bytes.NewReader(body))
		req = req.WithContext(auth.ContextWithClaims(ctx, interno(sub)))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Sin membresía: 401, no toca el reclamo.
	if rec := reclamar(otro, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reclamar sin membresía: status %d, quiere 401; cuerpo %s", rec.Code, rec.Body.String())
	}

	// Con membresía, primer reclamo (versión esperada 0): 201 con el Reclamo.
	rec := reclamar(ejec, 0)
	if rec.Code != http.StatusCreated {
		t.Fatalf("primer reclamo: status %d, quiere 201; cuerpo %s", rec.Code, rec.Body.String())
	}
	var r msgclient.Reclamo
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decodificar Reclamo: %v; cuerpo %s", err, rec.Body.String())
	}
	if r.Version != 1 {
		t.Fatalf("primer reclamo: version %d, quiere 1", r.Version)
	}
	if r.ResponsableId.String() != ejec {
		t.Fatalf("primer reclamo: responsable %s, quiere %s", r.ResponsableId, ejec)
	}

	// Reclamar otra vez con la versión vieja (0): conflicto de versión, 409.
	if rec := reclamar(ejec, 0); rec.Code != http.StatusConflict {
		t.Fatalf("reclamo con versión vieja: status %d, quiere 409; cuerpo %s", rec.Code, rec.Body.String())
	}
}
