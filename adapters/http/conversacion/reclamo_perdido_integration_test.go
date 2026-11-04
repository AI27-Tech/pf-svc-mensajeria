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
	rpTenant  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" // tenant = cliente (DEC-112)
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
			Subject: sub, TenantID: "ai27", Rol: "ejecutivo", Audiencia: auth.AudienciaInterno,
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
// prueban las dos escrituras del reclamo: enviar y liberar. La variante A→B→A
// está en TestDobleTransferenciaVersionObsoletaHTTP.
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
	if nuevo.Version != 2 {
		t.Fatalf("reclamo tras transferir: version %d, quiere 2", nuevo.Version)
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

// rpReclamo decodifica el Reclamo de una respuesta 201.
func rpReclamo(t *testing.T, paso string, rec *httptest.ResponseRecorder) msgclient.Reclamo {
	t.Helper()
	var r msgclient.Reclamo
	if err := json.Unmarshal(rpEsperar(t, paso, rec, http.StatusCreated), &r); err != nil {
		t.Fatalf("%s: decodificar Reclamo: %v", paso, err)
	}
	return r
}

// rpProhibido falla si la respuesta no es el 403 de la taxonomía.
func rpProhibido(t *testing.T, paso string, rec *httptest.ResponseRecorder) {
	t.Helper()
	var e msgclient.Error
	if err := json.Unmarshal(rpEsperar(t, paso, rec, http.StatusForbidden), &e); err != nil {
		t.Fatalf("%s: decodificar Error: %v", paso, err)
	}
	if e.Codigo != "prohibido" {
		t.Fatalf("%s: codigo %q, quiere prohibido", paso, e.Codigo)
	}
}

// rpVersionConversacion lee version_reclamo y el responsable activo ("" si está
// en el pool) desde obtenerConversacion.
func rpVersionConversacion(t *testing.T, do func(string, string, string, any) *httptest.ResponseRecorder) (int64, string) {
	t.Helper()
	var c msgclient.Conversacion
	if err := json.Unmarshal(rpEsperar(t, "obtener", do(http.MethodGet, "/api/v1/conversaciones/"+rpConv, rpOrigen, nil), http.StatusOK), &c); err != nil {
		t.Fatalf("decodificar Conversacion: %v", err)
	}
	if c.ReclamoActivo == nil {
		return c.VersionReclamo, ""
	}
	return c.VersionReclamo, c.ReclamoActivo.ResponsableId.String()
}

func rpTransferir(do func(string, string, string, any) *httptest.ResponseRecorder, origen, destino string, version int64) *httptest.ResponseRecorder {
	return do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/transferencias", origen,
		msgclient.TransferenciaEntrada{ResponsableDestinoId: uuid.MustParse(destino), VersionEsperada: version})
}

func rpLiberar(do func(string, string, string, any) *httptest.ResponseRecorder, sub string, r msgclient.Reclamo, version int64) *httptest.ResponseRecorder {
	return do(http.MethodDelete, "/api/v1/conversaciones/"+rpConv+"/reclamos/"+r.Id.String()+"?version_esperada="+strconv.FormatInt(version, 10), sub, nil)
}

// TestDobleTransferenciaVersionObsoletaHTTP cubre P20 en la variante A→B→A: la
// versión del reclamo avanza en cada transferencia (1, 2, 3), así que el envío y
// el reclamar de A con su versión previa a las transferencias responden 409 y no
// tienen efecto, aunque A vuelva a ser el responsable.
func TestDobleTransferenciaVersionObsoletaHTTP(t *testing.T) {
	do := rpServidor(t)

	a1 := rpReclamar(t, do, rpOrigen)
	b2 := rpReclamo(t, "A→B", rpTransferir(do, rpOrigen, rpDestino, a1.Version))
	a3 := rpReclamo(t, "B→A", rpTransferir(do, rpDestino, rpOrigen, b2.Version))
	if got := [3]int64{a1.Version, b2.Version, a3.Version}; got != [3]int64{1, 2, 3} {
		t.Fatalf("versiones A→B→A: %v, quiere [1 2 3]", got)
	}

	rpConflicto(t, "A envía con la versión previa", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/mensajes", rpOrigen,
		rpEnvio("en vuelo", a1.Version)))
	rpConflicto(t, "A reclama con la versión previa", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/reclamos", rpOrigen,
		msgclient.VersionEsperadaEntrada{VersionEsperada: a1.Version}))
	rpConflicto(t, "B reclama con su versión obsoleta", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/reclamos", rpDestino,
		msgclient.VersionEsperadaEntrada{VersionEsperada: b2.Version}))

	if v, resp := rpVersionConversacion(t, do); v != 3 || resp != rpOrigen {
		t.Fatalf("tras los rechazos: version %d responsable %s, quiere 3 %s", v, resp, rpOrigen)
	}
	var pagina msgclient.PaginaMensajes
	if err := json.Unmarshal(rpEsperar(t, "listar", do(http.MethodGet, "/api/v1/conversaciones/"+rpConv+"/mensajes", rpOrigen, nil), http.StatusOK), &pagina); err != nil {
		t.Fatalf("decodificar página: %v", err)
	}
	if len(pagina.Items) != 0 {
		t.Fatalf("el envío rechazado no debe dejar mensaje, hay %d", len(pagina.Items))
	}

	rpEsperar(t, "A envía con la versión vigente", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/mensajes", rpOrigen,
		rpEnvio("vigente", a3.Version)), http.StatusCreated)
}

// TestSoloElResponsableLiberaYTransfiereHTTP: solo el responsable activo libera
// o transfiere (DEC-118); otro miembro del pool, aun con la versión vigente,
// recibe 403 sin efecto. Tras liberar, la versión de la conversación se conserva
// (no vuelve a 0) y el siguiente reclamo la avanza.
func TestSoloElResponsableLiberaYTransfiereHTTP(t *testing.T) {
	do := rpServidor(t)

	a1 := rpReclamar(t, do, rpOrigen)
	rpProhibido(t, "B libera el reclamo de A", rpLiberar(do, rpDestino, a1, a1.Version))
	rpProhibido(t, "B transfiere el reclamo de A", rpTransferir(do, rpDestino, rpDestino, a1.Version))
	if v, resp := rpVersionConversacion(t, do); v != 1 || resp != rpOrigen {
		t.Fatalf("tras los 403: version %d responsable %s, quiere 1 %s", v, resp, rpOrigen)
	}

	b2 := rpReclamo(t, "A transfiere a B", rpTransferir(do, rpOrigen, rpDestino, a1.Version))
	rpProhibido(t, "A libera el reclamo de B con la versión vigente", rpLiberar(do, rpOrigen, b2, b2.Version))
	rpConflicto(t, "A libera con su versión obsoleta", rpLiberar(do, rpOrigen, a1, a1.Version))

	rpEsperar(t, "B libera", rpLiberar(do, rpDestino, b2, b2.Version), http.StatusNoContent)
	if v, resp := rpVersionConversacion(t, do); v != 2 || resp != "" {
		t.Fatalf("tras liberar: version %d responsable %q, quiere 2 y en el pool", v, resp)
	}
	var trabajo msgclient.TrabajoBandeja
	if err := json.Unmarshal(rpEsperar(t, "bandeja", do(http.MethodGet, "/api/v1/bandeja/trabajo", rpOrigen, nil), http.StatusOK), &trabajo); err != nil {
		t.Fatalf("decodificar TrabajoBandeja: %v", err)
	}
	if len(trabajo.Items) != 1 || trabajo.Items[0].VersionReclamo != 2 {
		t.Fatalf("bandeja tras liberar: %+v, quiere 1 conversación con version_reclamo 2", trabajo.Items)
	}

	rpConflicto(t, "reclamar con versión 0 tras liberar", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/reclamos", rpOrigen,
		msgclient.VersionEsperadaEntrada{VersionEsperada: 0}))
	a3 := rpReclamo(t, "A reclama con la versión vigente", do(http.MethodPost, "/api/v1/conversaciones/"+rpConv+"/reclamos", rpOrigen,
		msgclient.VersionEsperadaEntrada{VersionEsperada: 2}))
	if a3.Version != 3 {
		t.Fatalf("reclamo tras liberar: version %d, quiere 3", a3.Version)
	}
}
