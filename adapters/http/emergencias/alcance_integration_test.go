package emergencias_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/google/uuid"

	"github.com/AI27-Tech/pf-svc-mensajeria/internal/alcance"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
)

// TestAlcanceMonitoreoHTTP cubre el alcance por cliente de las emergencias del
// viaje (DEC-149 #6, #18; mensajeria.yaml de tracer.42) por HTTP:
//   - monitoreo con la empresa A en sus clientes: lista, activa, anota y cierra
//     los casos del viaje de A;
//   - sobre el viaje de la empresa B: 404 al listar, activar (sin crear caso),
//     anotar (también con la ruta del viaje de A y el caso de B) y cerrar por
//     viaje y por id; el caso no cambia;
//   - Supervisor libera el caso de B por la ruta del viaje: 404; por id (la
//     bandeja de Reacción no cambia): 204. Reacción no se acota;
//   - el administrador opera sobre todo;
//   - la proyección manda: un alcance.cambiado más nuevo cambia lo que ve el
//     Monitorista, y el conjunto vacío lo deja sin nada.
//
// Se salta sin MENSAJERIA_TEST_DB.
func TestAlcanceMonitoreoHTTP(t *testing.T) {
	e := montar(t)
	const viajeB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	dbtest.Exec(t, e.ctx, e.admin, `INSERT INTO mensajeria.conversacion (tenant_id, viaje_id, cliente_id)
		 VALUES ('`+otraEmp+`', '`+viajeB+`', '`+otraEmp+`')`)
	ctxB := auth.ContextWithClaims(e.ctx,
		&auth.Claims{Subject: clienteU, TenantID: otraEmp, Rol: "cliente", Audiencia: auth.AudienciaCliente})
	casoB, _, err := e.mod.Emergencias().ActivarEmergencia(ctxB, uuid.MustParse(viajeB),
		msgclient.ActivarEmergenciaEntrada{ActivacionId: uuid.New()})
	if err != nil {
		t.Fatalf("la empresa B activa: %v", err)
	}
	rutaA := "/api/v1/viajes/" + emViaje + "/emergencias"
	rutaB := "/api/v1/viajes/" + viajeB + "/emergencias"
	idB := casoB.Id.String()

	quiere := func(nombre string, rec *httptest.ResponseRecorder, status int) {
		t.Helper()
		if rec.Code != status {
			t.Fatalf("%s: status %d, quiere %d; cuerpo %s", nombre, rec.Code, status, rec.Body.String())
		}
	}
	nota := msgclient.NotaEmergenciaEntrada{Cuerpo: "unidad detenida"}
	activacion := func() msgclient.ActivarEmergenciaEntrada {
		return msgclient.ActivarEmergenciaEntrada{ActivacionId: uuid.New()}
	}
	contar := func(viaje string) int {
		t.Helper()
		var n int
		if err := e.admin.QueryRow(e.ctx, `SELECT count(*) FROM mensajeria.emergencia WHERE viaje_id = $1`, viaje).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	estado := func(id string) string {
		t.Helper()
		var s string
		if err := e.admin.QueryRow(e.ctx, `SELECT estado FROM mensajeria.emergencia WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Dentro del alcance: el viaje de A.
	quiere("Monitorista lista A", e.pedir(monit, "monitorista", http.MethodGet, rutaA, nil), http.StatusOK)
	quiere("Monitorista activa A", e.pedir(monit, "monitorista", http.MethodPost, rutaA, activacion()), http.StatusCreated)
	quiere("Monitorista anota A", e.pedir(monit, "monitorista", http.MethodPost, rutaA+"/"+e.caso+"/notas", nota), http.StatusCreated)

	// Fuera del alcance: el viaje de B, para los tres roles de monitoreo.
	for _, rol := range []string{"monitorista", "supervisor", "coordinador"} {
		sub := sujeto(rol)
		quiere(rol+" lista B", e.pedir(sub, rol, http.MethodGet, rutaB, nil), http.StatusNotFound)
		quiere(rol+" activa B", e.pedir(sub, rol, http.MethodPost, rutaB, activacion()), http.StatusNotFound)
		quiere(rol+" anota B", e.pedir(sub, rol, http.MethodPost, rutaB+"/"+idB+"/notas", nota), http.StatusNotFound)
		quiere(rol+" anota B por la ruta de A", e.pedir(sub, rol, http.MethodPost, rutaA+"/"+idB+"/notas", nota), http.StatusNotFound)
	}
	if n := contar(viajeB); n != 1 {
		t.Fatalf("casos de B = %d; quiere 1: la activación fuera de alcance no crea nada", n)
	}

	// Supervisor libera el reclamo de B: por la ruta del viaje se acota; por id
	// (bandeja de Reacción) no. Reacción reclama sin alcance.
	reclamar := func(id string, v int64) {
		t.Helper()
		quiere("Reacción reclama", e.pedir(reaccionA, "reaccion", http.MethodPost, "/api/v1/emergencias/"+id+"/reclamos",
			msgclient.VersionEsperadaEntrada{VersionEsperada: v}), http.StatusCreated)
	}
	reclamar(idB, 0)
	rid, _, v := e.activo(t)
	sufijo := "/reclamos/" + rid + "?version_esperada=" + strconv.FormatInt(v, 10)
	quiere("Supervisor libera B por viaje", e.pedir(superv, "supervisor", http.MethodDelete, rutaB+"/"+idB+sufijo, nil), http.StatusNotFound)
	quiere("Supervisor libera B por id", e.pedir(superv, "supervisor", http.MethodDelete, "/api/v1/emergencias/"+idB+sufijo, nil), http.StatusNoContent)

	// Resolver los dos casos para probar el cierre administrativo.
	resolver := func(id string, v int64) {
		t.Helper()
		reclamar(id, v)
		quiere("Reacción resuelve", e.pedir(reaccionA, "reaccion", http.MethodPost, "/api/v1/emergencias/"+id+"/resolucion",
			msgclient.ResolucionEmergenciaEntrada{VersionEsperada: v + 1, Nota: "localizada"}), http.StatusOK)
	}
	resolver(idB, 1)
	resolver(e.caso, 0)
	quiere("Monitorista cierra B por viaje", e.pedir(monit, "monitorista", http.MethodPost, rutaB+"/"+idB+"/cierre", nil), http.StatusNotFound)
	quiere("Coordinador cierra B por id", e.pedir(coord, "coordinador", http.MethodPost, "/api/v1/emergencias/"+idB+"/cierre", nil), http.StatusNotFound)
	if s := estado(idB); s != "resuelta" {
		t.Fatalf("caso de B: %s; quiere resuelta, los rechazos no lo cierran", s)
	}
	quiere("Monitorista cierra A por viaje", e.pedir(monit, "monitorista", http.MethodPost, rutaA+"/"+e.caso+"/cierre", nil), http.StatusOK)

	// El administrador opera sobre todo.
	var pag msgclient.PaginaEmergencias
	rec := e.pedir(uuid.NewString(), "administrador", http.MethodGet, rutaB, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &pag); rec.Code != http.StatusOK || err != nil || len(pag.Items) != 1 {
		t.Fatalf("administrador lista B: status %d, %d casos; quiere 200 con 1", rec.Code, len(pag.Items))
	}
	quiere("administrador cierra B", e.pedir(uuid.NewString(), "administrador", http.MethodPost, "/api/v1/emergencias/"+idB+"/cierre", nil), http.StatusOK)

	// La proyección manda: un alcance.cambiado más nuevo da B al Monitorista y
	// le quita A (conjunto completo); el vacío lo deja sin nada.
	pool, closePool := dbtest.PoolComoRol(t, e.ctx)
	defer closePool()
	cons := alcance.NuevoConsumidor(alcance.NuevaProyeccion(pool))
	evento := func(en time.Time, clientes ...string) {
		t.Helper()
		if clientes == nil {
			clientes = []string{}
		}
		raw, _ := json.Marshal(map[string]any{"sujeto_id": uuid.NewString(), "idp_subject": monit,
			"clientes_efectivos_ids": clientes, "cambiado_en": en.UTC().Format(time.RFC3339Nano)})
		if err := cons.Procesar(e.ctx, bus.Sobre{Tipo: alcance.TipoAlcanceCambiado, Payload: raw}); err != nil {
			t.Fatalf("procesar alcance.cambiado: %v", err)
		}
	}
	futuro := time.Now().Add(time.Hour)
	evento(futuro, otraEmp)
	quiere("Monitorista lista B tras el evento", e.pedir(monit, "monitorista", http.MethodGet, rutaB, nil), http.StatusOK)
	quiere("Monitorista lista A tras el evento", e.pedir(monit, "monitorista", http.MethodGet, rutaA, nil), http.StatusNotFound)
	evento(futuro.Add(time.Minute))
	quiere("Monitorista sin clientes lista B", e.pedir(monit, "monitorista", http.MethodGet, rutaB, nil), http.StatusNotFound)
}
