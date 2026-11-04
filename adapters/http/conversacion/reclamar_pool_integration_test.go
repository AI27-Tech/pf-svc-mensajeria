package conversacion_test

import (
	"encoding/json"
	"net/http"
	"testing"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
)

// TestReclamarSoloDesdeElPoolHTTP cubre DEC-123: reclamar solo procede si la
// conversación no tiene reclamo activo. Otro miembro del pool con la versión
// vigente recibe 409 por HTTP y por la superficie en proceso, el reclamo sigue
// con su responsable y cada intento queda auditado como perdido. Del pool se
// sigue reclamando: tras liberar, el otro miembro toma la conversación en la
// versión siguiente. Se salta sin MENSAJERIA_TEST_DB.
func TestReclamarSoloDesdeElPoolHTTP(t *testing.T) {
	e := montarAtencion(t)
	reclamar := func(sub string, version int64) *http.Response {
		return e.pedir(sub, "ejecutivo", http.MethodPost, rutaConv+"/reclamos", msgclient.VersionEsperadaEntrada{VersionEsperada: version}).Result()
	}
	responsable := func() (string, int64) {
		t.Helper()
		var actor string
		var version int64
		if err := e.admin.QueryRow(e.ctx,
			`SELECT actor_real::text, version FROM mensajeria.reclamo WHERE cerrado_en IS NULL`).Scan(&actor, &version); err != nil {
			t.Fatalf("leer reclamo activo: %v", err)
		}
		return actor, version
	}

	// A reclama del pool: 201 en versión 1.
	if c := reclamar(atEjec, 0).StatusCode; c != http.StatusCreated {
		t.Fatalf("A reclama del pool: status %d, quiere 201", c)
	}

	// B, miembro del pool, con la versión vigente (1): 409 conflicto por HTTP.
	rec := e.pedir(atOtro, "ejecutivo", http.MethodPost, rutaConv+"/reclamos", msgclient.VersionEsperadaEntrada{VersionEsperada: 1})
	var errB msgclient.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &errB); rec.Code != http.StatusConflict || err != nil || errB.Codigo != "conflicto" {
		t.Fatalf("B reclama la de A por HTTP: status %d codigo %q, quiere 409 conflicto; cuerpo %s", rec.Code, errB.Codigo, rec.Body.String())
	}
	// Y por la superficie en proceso (adaptador de pf-svc-core): mismo conflicto.
	ctxB := auth.ContextWithClaims(e.ctx, &auth.Claims{Subject: atOtro, TenantID: "ai27", Rol: "ejecutivo", Audiencia: auth.AudienciaInterno})
	if _, err := e.mod.Conversaciones().ReclamarConversacion(ctxB, uuid.MustParse(atConv), msgclient.VersionEsperadaEntrada{VersionEsperada: 1}); platformerrors.AsCategory(err) != platformerrors.CategoryConflicto {
		t.Fatalf("B reclama la de A en proceso: err %v, quiere conflicto", err)
	}
	// El propio responsable tampoco re-reclama una conversación que ya tiene.
	if c := reclamar(atEjec, 1).StatusCode; c != http.StatusConflict {
		t.Fatalf("A re-reclama la suya: status %d, quiere 409", c)
	}
	if actor, v := responsable(); actor != atEjec || v != 1 {
		t.Fatalf("tras los 409: responsable %s versión %d, quiere %s 1", actor, v, atEjec)
	}

	// A libera; B reclama del pool con la versión vigente (1): 201 en versión 2.
	var rid string
	if err := e.admin.QueryRow(e.ctx, `SELECT id FROM mensajeria.reclamo WHERE cerrado_en IS NULL`).Scan(&rid); err != nil {
		t.Fatalf("leer id del reclamo: %v", err)
	}
	if c := e.pedir(atEjec, "ejecutivo", http.MethodDelete, rutaConv+"/reclamos/"+rid+"?version_esperada=1", nil).Code; c != http.StatusNoContent {
		t.Fatalf("A libera: status %d, quiere 204", c)
	}
	if c := reclamar(atOtro, 1).StatusCode; c != http.StatusCreated {
		t.Fatalf("B reclama del pool: status %d, quiere 201", c)
	}
	if actor, v := responsable(); actor != atOtro || v != 2 {
		t.Fatalf("tras reclamar del pool: responsable %s versión %d, quiere %s 2", actor, v, atOtro)
	}

	// Auditoría: cada intento, en orden.
	type fila struct {
		accion, actor           string
		esperada, actual, nueva any
	}
	rows, err := e.admin.Query(e.ctx,
		`SELECT accion, actor_real::text, detalle->'version_esperada', detalle->'version_actual', detalle->'version_reclamo'
		   FROM mensajeria.auditoria_mensajeria ORDER BY id`)
	if err != nil {
		t.Fatalf("leer auditoría: %v", err)
	}
	var got []fila
	for rows.Next() {
		var f fila
		if err := rows.Scan(&f.accion, &f.actor, &f.esperada, &f.actual, &f.nueva); err != nil {
			t.Fatalf("escanear auditoría: %v", err)
		}
		got = append(got, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterar auditoría: %v", err)
	}
	quiere := []fila{
		{"reclamo.intento_ganado", atEjec, 0.0, nil, 1.0},
		{"reclamo.intento_perdido", atOtro, 1.0, 1.0, nil},
		{"reclamo.intento_perdido", atOtro, 1.0, 1.0, nil},
		{"reclamo.intento_perdido", atEjec, 1.0, 1.0, nil},
		{"reclamo.liberado", atEjec, nil, nil, 1.0},
		{"reclamo.intento_ganado", atOtro, 1.0, nil, 2.0},
	}
	if len(got) != len(quiere) {
		t.Fatalf("auditoría: %d filas %v, quiere %d %v", len(got), got, len(quiere), quiere)
	}
	for i := range quiere {
		if got[i] != quiere[i] {
			t.Fatalf("auditoría fila %d: %v, quiere %v (todas: %v)", i, got[i], quiere[i], got)
		}
	}
}
