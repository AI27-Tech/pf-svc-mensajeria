package bandejas_test

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

// TestListarTrabajoBandejaHTTP prueba el handler de punta a punta contra Postgres
// real: autorización por membresía de pool, bootstrap (items + revisión) y
// catch-up incremental por desde_revision. Llama al handler como lo llama bff
// (claims en contexto).
func TestListarTrabajoBandejaHTTP(t *testing.T) {
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
		bandeja = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		convA   = "11111111-1111-1111-1111-111111111111"
		convB   = "22222222-2222-2222-2222-222222222222"
		cliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		ejec    = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		otro    = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre, revision)
		 VALUES ('`+bandeja+`', '`+tenant+`', 'chat', 'cliente A', 2)`)
	for _, c := range []string{convA, convB} {
		dbtest.Exec(t, ctx, admin,
			`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
			 VALUES ('`+c+`', '`+tenant+`', gen_random_uuid(), '`+cliente+`', 'abierta', 0)`)
	}
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa, revision)
		 VALUES ('`+convA+`', '`+tenant+`', '`+bandeja+`', true, 1),
		        ('`+convB+`', '`+tenant+`', '`+bandeja+`', true, 2)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, version, activa)
		 VALUES ('`+bandeja+`', '`+ejec+`', '`+tenant+`', 'ejecutivo', 1, true)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{})
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	mux := http.NewServeMux()
	mod.Routes(mux)

	interno := func(sub string) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: tenant, Rol: "ejecutivo", Audiencia: auth.AudienciaInterno}
	}
	get := func(sub, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/bandejas/"+bandeja+"/trabajo"+query, nil)
		req = req.WithContext(auth.ContextWithClaims(ctx, interno(sub)))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Sin membresía: 401.
	if rec := get(otro, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sin membresía: status %d, quiere 401; cuerpo %s", rec.Code, rec.Body.String())
	}

	// Bootstrap: 200 con las dos conversaciones y revisión 2.
	rec := get(ejec, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap: status %d, quiere 200; cuerpo %s", rec.Code, rec.Body.String())
	}
	var tb msgclient.TrabajoBandeja
	if err := json.Unmarshal(rec.Body.Bytes(), &tb); err != nil {
		t.Fatalf("decodificar TrabajoBandeja: %v; cuerpo %s", err, rec.Body.String())
	}
	if tb.Revision != 2 {
		t.Fatalf("revisión de bootstrap = %d, quiere 2", tb.Revision)
	}
	if len(tb.Items) != 2 {
		t.Fatalf("bootstrap trae %d conversaciones, quiere 2", len(tb.Items))
	}

	// Catch-up desde 1: solo convB.
	rec2 := get(ejec, "?desde_revision=1")
	if rec2.Code != http.StatusOK {
		t.Fatalf("catch-up: status %d, quiere 200; cuerpo %s", rec2.Code, rec2.Body.String())
	}
	var tb2 msgclient.TrabajoBandeja
	if err := json.Unmarshal(rec2.Body.Bytes(), &tb2); err != nil {
		t.Fatalf("decodificar catch-up: %v", err)
	}
	if len(tb2.Items) != 1 || tb2.Items[0].Id.String() != convB {
		t.Fatalf("catch-up desde 1 debe traer solo convB, trajo %d items", len(tb2.Items))
	}
}
