package conversacion_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformpgx "github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/google/uuid"
)

const (
	rpTenant  = "t1"
	rpConv    = "11111111-1111-1111-1111-111111111111"
	rpCliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	rpBandeja = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	rpOrigen  = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	rpDestino = "ffffffff-ffff-ffff-ffff-ffffffffffff"
)

// rpServidor monta mensajeria contra Postgres real con una conversación abierta
// en una bandeja de chat cuyo pool tiene a origen y destino como miembros
// vigentes, sin reclamo. Devuelve la función que llama al mux como bff (claims
// del sujeto en contexto). Se salta sin MENSAJERIA_TEST_DB.
func rpServidor(t *testing.T) func(method, path, sub string, body any) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	t.Cleanup(closeAdmin)
	dbtest.Reiniciar(t, ctx, admin)
	t.Cleanup(func() { dbtest.Reiniciar(t, ctx, admin) })
	dbtest.AplicarMigraciones(t, ctx, admin)

	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+rpConv+`', '`+rpTenant+`', gen_random_uuid(), '`+rpCliente+`', 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre)
		 VALUES ('`+rpBandeja+`', '`+rpTenant+`', 'chat', 'cliente A')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa)
		 VALUES ('`+rpConv+`', '`+rpTenant+`', '`+rpBandeja+`', true)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, version, activa)
		 VALUES ('`+rpBandeja+`', '`+rpOrigen+`', '`+rpTenant+`', 'ejecutivo', 1, true),
		        ('`+rpBandeja+`', '`+rpDestino+`', '`+rpTenant+`', 'ejecutivo', 1, true)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	t.Cleanup(closePool)
	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{})
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	mux := http.NewServeMux()
	mod.Routes(mux)

	return func(method, path, sub string, body any) *httptest.ResponseRecorder {
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req = req.WithContext(auth.ContextWithClaims(ctx, &auth.Claims{
			Subject: sub, TenantID: rpTenant, Rol: "ejecutivo", Audiencia: auth.AudienciaInterno,
		}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
}

// rpEnvio arma un EnviarMensajeEntrada con la versión de reclamo dada.
func rpEnvio(cuerpo string, version int64) msgclient.EnviarMensajeEntrada {
	return msgclient.EnviarMensajeEntrada{ClientMessageId: uuid.New(), Cuerpo: cuerpo, VersionReclamoEsperada: &version}
}

// rpEsperar falla si el status no es el esperado; devuelve el cuerpo.
func rpEsperar(t *testing.T, paso string, rec *httptest.ResponseRecorder, status int) []byte {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("%s: status %d, quiere %d; cuerpo %s", paso, rec.Code, status, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// rpConflicto falla si la respuesta no es el 409 de la taxonomía (codigo conflicto).
func rpConflicto(t *testing.T, paso string, rec *httptest.ResponseRecorder) {
	t.Helper()
	var e msgclient.Error
	if err := json.Unmarshal(rpEsperar(t, paso, rec, http.StatusConflict), &e); err != nil {
		t.Fatalf("%s: decodificar Error: %v", paso, err)
	}
	if e.Codigo != "conflicto" {
		t.Fatalf("%s: codigo %q, quiere conflicto", paso, e.Codigo)
	}
}

// rpReclamar toma el reclamo (versión esperada 0) y devuelve el Reclamo.
func rpReclamar(t *testing.T, do func(string, string, string, any) *httptest.ResponseRecorder, sub string) msgclient.Reclamo {
	t.Helper()
	var r msgclient.Reclamo
	body := rpEsperar(t, "reclamar", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/reclamos", sub,
		msgclient.VersionEsperadaEntrada{VersionEsperada: 0}), http.StatusCreated)
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decodificar Reclamo: %v", err)
	}
	return r
}

// TestVersionAnteriorATransferenciaHTTP cubre P20 (E3-T3) en su escenario de
// una transferencia: una escritura en vuelo del responsable anterior con la
// versión de reclamo previa se rechaza con 409, nunca se aplica en silencio. Se
// prueban las dos escrituras del reclamo: enviar y liberar.
//
// Límite conocido: hoy Transferir reabre el reclamo del destino en versión 1, la
// misma que la previa. El 409 del envío lo produce la guarda de actor (el reclamo
// activo es del destino), no la versión; tras A→B→A un envío con la versión
// previa entra (201). Esa variante no se prueba aquí hasta que Transferir avance
// la versión. La prueba usa la versión que devuelve la transferencia, así que
// sigue válida cuando eso cambie.
func TestVersionAnteriorATransferenciaHTTP(t *testing.T) {
	do := rpServidor(t)

	previo := rpReclamar(t, do, rpOrigen)
	if previo.Version != 1 {
		t.Fatalf("reclamo previo: version %d, quiere 1", previo.Version)
	}

	var nuevo msgclient.Reclamo
	if err := json.Unmarshal(rpEsperar(t, "transferir", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/transferencias", rpOrigen,
		msgclient.TransferenciaEntrada{ResponsableDestinoId: uuid.MustParse(rpDestino), VersionEsperada: previo.Version}),
		http.StatusCreated), &nuevo); err != nil {
		t.Fatalf("decodificar Reclamo: %v", err)
	}

	// Enviar con la versión previa: 409 y no se crea mensaje.
	rpConflicto(t, "enviar con versión previa", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/mensajes", rpOrigen,
		rpEnvio("en vuelo", int64(previo.Version))))

	// Liberar el reclamo previo con su versión: 409 y el reclamo del destino sigue.
	rpConflicto(t, "liberar con versión previa", do(http.MethodDelete,
		"/api/v1/conversaciones/"+rpConv+"/reclamos/"+previo.Id.String()+"?version_esperada="+strconv.FormatInt(int64(previo.Version), 10),
		rpOrigen, nil))

	var pagina msgclient.PaginaMensajes
	if err := json.Unmarshal(rpEsperar(t, "listar", do(http.MethodGet, "/api/v1/conversaciones/"+rpConv+"/mensajes", rpDestino, nil), http.StatusOK), &pagina); err != nil {
		t.Fatalf("decodificar página: %v", err)
	}
	if len(pagina.Items) != 0 {
		t.Fatalf("el envío rechazado no debe dejar mensaje, hay %d", len(pagina.Items))
	}

	// El destino sigue siendo el responsable: su envío con la versión que le dio la
	// transferencia entra.
	rpEsperar(t, "enviar del destino", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/mensajes", rpDestino,
		rpEnvio("del destino", int64(nuevo.Version))), http.StatusCreated)
}

// TestLecturaSinEnvioTrasPerderReclamoHTTP cubre E3-T3: tras perder el reclamo
// (por liberación o por transferencia), el ejecutivo anterior, con su membresía
// del pool vigente, sigue leyendo la conversación (obtener y listar mensajes, con
// el mensaje que él mismo envió) pero su envío se rechaza con 409.
func TestLecturaSinEnvioTrasPerderReclamoHTTP(t *testing.T) {
	casos := []struct {
		name   string
		perder func(t *testing.T, do func(string, string, string, any) *httptest.ResponseRecorder, r msgclient.Reclamo)
	}{
		{"liberación", func(t *testing.T, do func(string, string, string, any) *httptest.ResponseRecorder, r msgclient.Reclamo) {
			rpEsperar(t, "liberar", do(http.MethodDelete,
				"/api/v1/conversaciones/"+rpConv+"/reclamos/"+r.Id.String()+"?version_esperada="+strconv.FormatInt(int64(r.Version), 10), rpOrigen, nil),
				http.StatusNoContent)
		}},
		{"transferencia", func(t *testing.T, do func(string, string, string, any) *httptest.ResponseRecorder, r msgclient.Reclamo) {
			rpEsperar(t, "transferir", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/transferencias", rpOrigen,
				msgclient.TransferenciaEntrada{ResponsableDestinoId: uuid.MustParse(rpDestino), VersionEsperada: r.Version}),
				http.StatusCreated)
		}},
	}
	for _, tc := range casos {
		t.Run(tc.name, func(t *testing.T) {
			do := rpServidor(t)
			r := rpReclamar(t, do, rpOrigen)

			// Con el reclamo, el origen envía (control positivo de la guarda).
			rpEsperar(t, "enviar con reclamo", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/mensajes", rpOrigen,
				rpEnvio("antes de perder", int64(r.Version))), http.StatusCreated)

			tc.perder(t, do, r)

			var conv msgclient.Conversacion
			if err := json.Unmarshal(rpEsperar(t, "obtener", do(http.MethodGet, "/api/v1/conversaciones/"+rpConv, rpOrigen, nil), http.StatusOK), &conv); err != nil {
				t.Fatalf("decodificar Conversacion: %v", err)
			}
			if conv.Id.String() != rpConv {
				t.Fatalf("obtener: id %s, quiere %s", conv.Id, rpConv)
			}

			var pagina msgclient.PaginaMensajes
			if err := json.Unmarshal(rpEsperar(t, "listar", do(http.MethodGet, "/api/v1/conversaciones/"+rpConv+"/mensajes", rpOrigen, nil), http.StatusOK), &pagina); err != nil {
				t.Fatalf("decodificar página: %v", err)
			}
			if len(pagina.Items) != 1 || pagina.Items[0].Cuerpo != "antes de perder" {
				t.Fatalf("listar: quiere 1 mensaje 'antes de perder', hubo %d", len(pagina.Items))
			}

			rpConflicto(t, "enviar sin reclamo", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/mensajes", rpOrigen,
				rpEnvio("después de perder", int64(r.Version))))
		})
	}
}
