package emergencias_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/google/uuid"
)

// TestTransferirEmergenciaDestinoIdDeSujetoHTTP cubre DEC-148 #77 (DT-28) en las
// dos operaciones (por viaje y por id): un destino que resuelve como
// rol_sujeto.sujeto_id y NO como rol_sujeto.idp_subject es 409
// destino_invalido_en_viaje sin ningún efecto; un `sub`, conocido o no, pasa como
// antes. Valores a mano. Se salta sin MENSAJERIA_TEST_DB.
func TestTransferirEmergenciaDestinoIdDeSujetoHTTP(t *testing.T) {
	e := montar(t)
	cuenta := func(sql string) (n int) {
		t.Helper()
		if err := e.admin.QueryRow(e.ctx, sql).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sembrar := func(sujeto, sub string) {
		t.Helper()
		var idp any
		if sub != "" {
			idp = sub
		}
		if _, err := e.admin.Exec(e.ctx,
			`INSERT INTO mensajeria.rol_sujeto (sujeto_id, idp_subject, rol) VALUES ($1, $2, 'reaccion')`, sujeto, idp); err != nil {
			t.Fatal(err)
		}
	}
	// Las dos gemelas: la por viaje y la por id.
	porViaje := func(destino string, v int64) *httpResp { return mandar(e.transferir(superv, "supervisor", destino, v)) }
	porId := func(destino string, v int64) *httpResp {
		return mandar(e.pedir(superv, "supervisor", http.MethodPost, e.porID(e.caso)+"/transferencias",
			msgclient.TransferenciaEntrada{ResponsableDestinoId: uuid.MustParse(destino), VersionEsperada: v}))
	}
	rechazado := func(op string, f func(string, int64) *httpResp, motivo, destino string, v int64) {
		t.Helper()
		rid0, a0, v0 := e.activo(t)
		salidas, auditadas := cuenta(`SELECT count(*) FROM mensajeria.outbox`), cuenta(`SELECT count(*) FROM mensajeria.auditoria_mensajeria`)
		if r := f(destino, v); r.code != http.StatusConflict || r.codigo != "destino_invalido_en_viaje" {
			t.Errorf("%s/%s: %d %q %s; quiere 409 destino_invalido_en_viaje", op, motivo, r.code, r.codigo, r.cuerpo)
		}
		if rid, a, ver := e.activo(t); rid != rid0 || a != a0 || ver != v0 || ver != v {
			t.Errorf("%s/%s: el reclamo del llamante cambió: %s %q v%d (antes %s %q v%d)", op, motivo, rid, a, ver, rid0, a0, v0)
		}
		if n := cuenta(`SELECT count(*) FROM mensajeria.outbox`); n != salidas {
			t.Errorf("%s/%s: outbox pasó de %d a %d", op, motivo, salidas, n)
		}
		if n := cuenta(`SELECT count(*) FROM mensajeria.auditoria_mensajeria`); n != auditadas {
			t.Errorf("%s/%s: auditoría pasó de %d a %d", op, motivo, auditadas, n)
		}
		if n := cuenta(`SELECT count(*) FROM mensajeria.reclamo_emergencia`); n != int(v0) {
			t.Errorf("%s/%s: hay %d reclamos, quiere %d", op, motivo, n, v0)
		}
	}
	aceptado := func(op string, f func(string, int64) *httpResp, motivo, destino string, v int64) {
		t.Helper()
		if r := f(destino, v); r.code != http.StatusCreated {
			t.Fatalf("%s/%s: %d %s; quiere 201", op, motivo, r.code, r.cuerpo)
		}
		if _, a, ver := e.activo(t); a != destino || ver != v+1 {
			t.Fatalf("%s/%s: responsable %q v%d, quiere %q v%d", op, motivo, a, ver, destino, v+1)
		}
	}

	if r := e.reclamar(reaccionA, "reaccion", 0); r.Code != http.StatusCreated {
		t.Fatalf("A reclama: %d %s", r.Code, r.Body.String())
	}
	// Sujeto S1 con sub P1 (distintos), sujeto huérfano sin sub, y un valor que es
	// sujeto de una fila y sub de otra.
	s1, p1, huerfano, ambos := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	sembrar(s1, p1)
	sembrar(huerfano, "")
	sembrar(ambos, "")
	sembrar(uuid.NewString(), ambos)

	for _, op := range []struct {
		nombre string
		f      func(string, int64) *httpResp
	}{{"por viaje", porViaje}, {"por id", porId}} {
		rechazado(op.nombre, op.f, "id de sujeto con sub", s1, 1)
		rechazado(op.nombre, op.f, "id de sujeto sin sub conocido", huerfano, 1)
	}

	// La guarda corre DESPUÉS de tomarActivo: con versión vencida o sin ser el
	// responsable ni tener mando, el id de sujeto responde igual que un `sub`
	// (conflicto de versión / 403), nunca destino_invalido_en_viaje; si no, se
	// sondearía qué valores son ids de sujeto. Valores esperados a mano.
	for _, op := range []struct {
		nombre string
		f      func(string, int64) *httpResp
	}{{"por viaje", porViaje}, {"por id", porId}} {
		for _, destino := range []struct{ motivo, id string }{{"id de sujeto", s1}, {"sub", uuid.NewString()}} {
			if r := op.f(destino.id, 99); r.code != http.StatusConflict || r.codigo != "conflicto_version_reclamo" {
				t.Errorf("%s/versión vieja/%s: %d %q %s; quiere 409 conflicto_version_reclamo", op.nombre, destino.motivo, r.code, r.codigo, r.cuerpo)
			}
		}
	}
	sinPermiso := func(destino string) *httpResp {
		return mandar(e.transferir(reaccionB, "reaccion", destino, 1))
	}
	sinPermisoPorId := func(destino string) *httpResp {
		return mandar(e.pedir(reaccionB, "reaccion", http.MethodPost, e.porID(e.caso)+"/transferencias",
			msgclient.TransferenciaEntrada{ResponsableDestinoId: uuid.MustParse(destino), VersionEsperada: 1}))
	}
	for nombre, f := range map[string]func(string) *httpResp{"por viaje": sinPermiso, "por id": sinPermisoPorId} {
		for _, destino := range []struct{ motivo, id string }{{"id de sujeto", s1}, {"sub", uuid.NewString()}} {
			if r := f(destino.id); r.code != http.StatusForbidden || r.codigo != "prohibido" {
				t.Errorf("%s/sin permiso/%s: %d %q %s; quiere 403 prohibido", nombre, destino.motivo, r.code, r.codigo, r.cuerpo)
			}
		}
	}
	if _, a, ver := e.activo(t); a != reaccionA || ver != 1 {
		t.Fatalf("tras los rechazos previos: responsable %q v%d, quiere %q v1", a, ver, reaccionA)
	}

	// Cada aceptación avanza la versión; se alternan las dos operaciones.
	aceptado("por viaje", porViaje, "sub conocido", p1, 1)
	rechazado("por id", porId, "id de sujeto tras transferir", s1, 2)
	aceptado("por id", porId, "sub desconocido", uuid.NewString(), 2)
	aceptado("por viaje", porViaje, "ni sujeto ni sub", uuid.NewString(), 3)
	aceptado("por id", porId, "sujeto de uno y sub de otro", ambos, 4)
	aceptado("por viaje", porViaje, "sujeto de uno y sub de otro (viaje)", uuid.NewString(), 5)
}

type httpResp struct {
	code   int
	codigo string
	cuerpo string
}

func mandar(rec *httptest.ResponseRecorder) *httpResp {
	var er msgclient.Error
	_ = json.Unmarshal(rec.Body.Bytes(), &er)
	return &httpResp{rec.Code, er.Codigo, rec.Body.String()}
}
