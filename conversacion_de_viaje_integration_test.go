package mensajeria_test

import (
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

// TestObtenerConversacionDeViaje resuelve la conversación por viaje_id
// (obtenerConversacionDeViaje) por HTTP y por la superficie en proceso, caso por
// caso, y exige el mismo resultado en las dos vías: la no cerrada si existe; si
// todas están cerradas, la más reciente; sin conversación o sin acceso, 404.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestObtenerConversacionDeViaje(t *testing.T) {
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
		cliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		bandeja = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		ejec    = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		extrano = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"

		viajeAbierto   = "10000000-0000-0000-0000-000000000001"
		viajeCerrado   = "10000000-0000-0000-0000-000000000002"
		viajeSin       = "10000000-0000-0000-0000-000000000003"
		convVieja      = "20000000-0000-0000-0000-000000000001" // cerrada, viajeAbierto
		convAbierta    = "20000000-0000-0000-0000-000000000002" // abierta, viajeAbierto
		convCerradaA   = "20000000-0000-0000-0000-000000000003" // cerrada antigua, viajeCerrado
		convCerradaB   = "20000000-0000-0000-0000-000000000004" // cerrada reciente, viajeCerrado
		convOtroTenant = "20000000-0000-0000-0000-000000000005" // abierta, viajeSin en t2
	)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia, creado_en) VALUES
		 ('`+convVieja+`',     '`+tenant+`', '`+viajeAbierto+`', '`+cliente+`', 'cerrada', 0, now() - interval '3 days'),
		 ('`+convAbierta+`',   '`+tenant+`', '`+viajeAbierto+`', '`+cliente+`', 'abierta', 4, now() - interval '1 day'),
		 ('`+convCerradaA+`',  '`+tenant+`', '`+viajeCerrado+`', '`+cliente+`', 'cerrada', 0, now() - interval '5 days'),
		 ('`+convCerradaB+`',  '`+tenant+`', '`+viajeCerrado+`', '`+cliente+`', 'cerrada', 2, now() - interval '2 days'),
		 ('`+convOtroTenant+`', 't2',         '`+viajeSin+`',     '`+cliente+`', 'abierta', 0, now())`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre) VALUES ('`+bandeja+`', '`+tenant+`', 'chat', 'cliente A')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa)
		 VALUES ('`+convAbierta+`', '`+tenant+`', '`+bandeja+`', true)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{})
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	api := mod.Conversaciones()
	mux := http.NewServeMux()
	mod.Routes(mux) // además prueba que la ruta no choca con /viajes/{id}/emergencias

	como := func(sub, tenantID, rol string, aud auth.Audiencia) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: tenantID, Rol: rol, Audiencia: aud}
	}
	clienteT1 := como(cliente, tenant, "cliente", auth.AudienciaCliente)

	casos := []struct {
		nombre   string
		claims   *auth.Claims
		viaje    string
		status   int
		convID   string
		ultimaSq int64
	}{
		{"abierta, cliente dueño", clienteT1, viajeAbierto, http.StatusOK, convAbierta, 4},
		{"abierta, ejecutivo sin filas de membresía", como(ejec, tenant, "ejecutivo", auth.AudienciaInterno), viajeAbierto, http.StatusOK, convAbierta, 4},
		{"todas cerradas: la más reciente", clienteT1, viajeCerrado, http.StatusOK, convCerradaB, 2},
		{"sin conversación", clienteT1, viajeSin, http.StatusNotFound, "", 0},
		{"otro tenant", como(cliente, "t2", "cliente", auth.AudienciaCliente), viajeAbierto, http.StatusNotFound, "", 0},
		// DEC-124: otro usuario de la misma empresa (sub ≠ cliente_id) la lee.
		{"otro usuario de la empresa", como(uuid.NewString(), tenant, "cliente", auth.AudienciaCliente), viajeAbierto, http.StatusOK, convAbierta, 4},
		// DEC-127: un rol que no es Ejecutivo recibe el 404 de antes.
		{"interno monitorista", como(extrano, tenant, "monitorista", auth.AudienciaInterno), viajeAbierto, http.StatusNotFound, "", 0},
		{"interno sin ruta activa (cerrada)", como(ejec, tenant, "ejecutivo", auth.AudienciaInterno), viajeCerrado, http.StatusNotFound, "", 0},
		{"sin claims", nil, viajeAbierto, http.StatusUnauthorized, "", 0},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			ctxReq := ctx
			if c.claims != nil {
				ctxReq = auth.ContextWithClaims(ctx, c.claims)
			}

			req := httptest.NewRequest(http.MethodGet, "/api/v1/viajes/"+c.viaje+"/conversacion", nil).WithContext(ctxReq)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != c.status {
				t.Fatalf("HTTP: status %d, quiere %d; cuerpo %s", rec.Code, c.status, rec.Body.String())
			}

			conv, err := api.ObtenerConversacionDeViaje(ctxReq, uuid.MustParse(c.viaje))
			if c.status != http.StatusOK {
				if got := platformerrors.AsCategory(err).HTTPStatus(); got != c.status {
					t.Fatalf("en proceso: status %d (err %v), quiere %d", got, err, c.status)
				}
				return
			}
			if err != nil {
				t.Fatalf("en proceso: %v", err)
			}
			var porHTTP msgclient.Conversacion
			if err := json.Unmarshal(rec.Body.Bytes(), &porHTTP); err != nil {
				t.Fatalf("decodificar Conversacion: %v", err)
			}
			for via, got := range map[string]*msgclient.Conversacion{"HTTP": &porHTTP, "en proceso": conv} {
				if got.Id.String() != c.convID || got.ViajeId.String() != c.viaje || got.UltimaSecuencia != c.ultimaSq {
					t.Fatalf("%s: id %s viaje %s ultima_secuencia %d; quiere %s %s %d",
						via, got.Id, got.ViajeId, got.UltimaSecuencia, c.convID, c.viaje, c.ultimaSq)
				}
			}
		})
	}

	// Un {id} que no es UUID no nombra ningún viaje: 404, sin llegar a SQL.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/viajes/no-es-uuid/conversacion", nil).WithContext(auth.ContextWithClaims(ctx, clienteT1))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("viaje no UUID: status %d, quiere 404; cuerpo %s", rec.Code, rec.Body.String())
	}
}
