package emergencias_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/google/uuid"
)

const (
	ejec     = "99999999-9999-9999-9999-999999999999"
	noExiste = "dead0000-0000-0000-0000-00000000dead"
	otraEmp  = "0e000000-0000-0000-0000-0000000000e1" // tenant de otra empresa cliente
)

func (e escenario) porID(caso string) string { return "/api/v1/emergencias/" + caso }

// TestOperacionesPorIdHTTP recorre la bandeja de Reacción y el caso por su id
// (tracer.34) con las reglas de DEC-129 a DEC-132, sin filas de membresía:
//   - la bandeja la ven Reacción, Supervisor y Coordinador (403 para Ejecutivo y
//     404 para Cliente, DEC-132), sin datos del viaje, del más antiguo al más reciente y sin
//     cerrados;
//   - obtenerEmergencia trae viaje_id, cliente_id (la empresa) y el reclamo
//     activo; 403 a otro rol interno y al Cliente de la empresa, 404 al Cliente de
//     otra empresa y a un caso inexistente;
//   - las notas salen de la más antigua a la más reciente, con la de resolución;
//   - Supervisor no reclama ni resuelve (403) y libera con el reclamo que lee del
//     detalle; Reacción que no es responsable no resuelve (403); cierra el
//     monitoreo y no Reacción.
func TestOperacionesPorIdHTTP(t *testing.T) {
	e := montar(t)
	quiere := func(paso string, status, want int, cuerpo string) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: status %d, quiere %d; cuerpo %s", paso, status, want, cuerpo)
		}
	}
	get := func(sub, rol, ruta string, v any) int {
		t.Helper()
		rec := e.pedir(sub, rol, http.MethodGet, ruta, nil)
		if v != nil && rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
				t.Fatalf("decodificar %s: %v; cuerpo %s", ruta, err, rec.Body.String())
			}
		}
		return rec.Code
	}

	// Un segundo caso, posterior, y una nota del cliente en el primero.
	rec := e.pedir(clienteU, "cliente", http.MethodPost, "/api/v1/viajes/"+emViaje+"/emergencias",
		msgclient.ActivarEmergenciaEntrada{ActivacionId: uuid.New()})
	var segundo msgclient.CasoEmergencia
	if err := json.Unmarshal(rec.Body.Bytes(), &segundo); rec.Code != http.StatusCreated || err != nil {
		t.Fatalf("activar el segundo: status %d cuerpo %s", rec.Code, rec.Body.String())
	}
	quiere("nota del cliente", e.pedir(clienteU, "cliente", http.MethodPost, e.ruta()+"/notas",
		msgclient.NotaEmergenciaEntrada{Cuerpo: "  sigo en la caseta  "}).Code, http.StatusCreated, "")

	// Bandeja: Reacción y Supervisor la ven en orden de activación, sin viaje.
	for _, rol := range []string{"reaccion", "supervisor", "coordinador"} {
		var pag msgclient.PaginaBandejaEmergencias
		quiere("bandeja como "+rol, get(reaccionA, rol, "/api/v1/bandeja/emergencias", &pag), http.StatusOK, "")
		var ids []string
		for _, it := range pag.Items {
			ids = append(ids, it.Id.String())
		}
		if want := []string{e.caso, segundo.Id.String()}; !slices.Equal(ids, want) {
			t.Fatalf("bandeja como %s = %v, quiere %v", rol, ids, want)
		}
	}
	crudo := e.pedir(reaccionA, "reaccion", http.MethodGet, "/api/v1/bandeja/emergencias", nil).Body.String()
	if strings.Contains(crudo, "viaje_id") || strings.Contains(crudo, "cliente_id") {
		t.Fatalf("la bandeja de Reacción no lleva viaje ni cliente: %s", crudo)
	}
	quiere("bandeja como ejecutivo", get(ejec, "ejecutivo", "/api/v1/bandeja/emergencias", nil), http.StatusForbidden, "")
	quiere("bandeja como cliente", get(clienteU, "cliente", "/api/v1/bandeja/emergencias", nil), http.StatusNotFound, "")

	// Detalle: 200 a Reacción y Supervisor con viaje y empresa; 403 / 404 al resto.
	var caso msgclient.CasoEmergencia
	quiere("detalle como supervisor", get(superv, "supervisor", e.porID(e.caso), &caso), http.StatusOK, "")
	if caso.ViajeId.String() != emViaje || caso.ClienteId.String() != emTenant || caso.Estado != "abierta" || caso.ReclamoActivo != nil {
		t.Fatalf("detalle: viaje %s cliente %s estado %s reclamo %v; quiere %s %s abierta sin reclamo",
			caso.ViajeId, caso.ClienteId, caso.Estado, caso.ReclamoActivo, emViaje, emTenant)
	}
	quiere("detalle como reaccion", get(reaccionB, "reaccion", e.porID(e.caso), nil), http.StatusOK, "")
	quiere("detalle como ejecutivo", get(ejec, "ejecutivo", e.porID(e.caso), nil), http.StatusForbidden, "")
	quiere("detalle como monitorista", get(monit, "monitorista", e.porID(e.caso), nil), http.StatusForbidden, "")
	quiere("detalle como cliente de la empresa", get(clienteU, "cliente", e.porID(e.caso), nil), http.StatusForbidden, "")
	quiere("detalle inexistente", get(reaccionA, "reaccion", e.porID(noExiste), nil), http.StatusNotFound, "")
	quiere("detalle con id que no es UUID", get(reaccionA, "reaccion", e.porID("no-es-uuid"), nil), http.StatusNotFound, "")
	otro := e.pedirComo(clienteU, otraEmp, http.MethodGet, e.porID(e.caso))
	quiere("detalle como cliente de otra empresa", otro.Code, http.StatusNotFound, otro.Body.String())

	// Reclamar: Supervisor 403; A reclama por id y recibe el reclamo completo.
	reclamarID := func(sub, rol string, v int64) (int, msgclient.Reclamo) {
		rec := e.pedir(sub, rol, http.MethodPost, e.porID(e.caso)+"/reclamos", msgclient.VersionEsperadaEntrada{VersionEsperada: v})
		var r msgclient.Reclamo
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return rec.Code, r
	}
	code, _ := reclamarID(superv, "supervisor", 0)
	quiere("Supervisor reclama", code, http.StatusForbidden, "")
	code, r := reclamarID(reaccionA, "reaccion", 0)
	quiere("A reclama por id", code, http.StatusCreated, "")
	if r.ResponsableId.String() != reaccionA || r.Version != 1 || r.ConversacionId.String() != e.caso || r.Id == uuid.Nil {
		t.Fatalf("reclamo: %+v; quiere responsable %s versión 1 sobre el caso %s", r, reaccionA, e.caso)
	}

	// Supervisor lee el reclamo en el detalle y lo libera por id.
	caso = msgclient.CasoEmergencia{}
	get(superv, "supervisor", e.porID(e.caso), &caso)
	if caso.ReclamoActivo == nil || caso.ReclamoActivo.Id != r.Id || caso.ReclamoActivo.Version != 1 || caso.Estado != "en_atencion" {
		t.Fatalf("detalle con reclamo: %+v; quiere en_atencion con el reclamo %s v1", caso, r.Id)
	}
	liberar := e.pedir(superv, "supervisor", http.MethodDelete,
		e.porID(e.caso)+"/reclamos/"+caso.ReclamoActivo.Id.String()+"?version_esperada="+strconv.FormatInt(caso.ReclamoActivo.Version, 10), nil)
	quiere("Supervisor libera por id", liberar.Code, http.StatusNoContent, liberar.Body.String())

	// Resolver: A reclama de nuevo; Supervisor y B (Reacción no responsable) 403;
	// A resuelve y recibe la vista de Reacción.
	code, _ = reclamarID(reaccionA, "reaccion", 1)
	quiere("A reclama de nuevo", code, http.StatusCreated, "")
	resolver := func(sub, rol string) *msgclient.EmergenciaReaccion {
		rec := e.pedir(sub, rol, http.MethodPost, e.porID(e.caso)+"/resolucion",
			msgclient.ResolucionEmergenciaEntrada{VersionEsperada: 2, Nota: "unidad localizada"})
		if rec.Code != http.StatusOK {
			return nil
		}
		var v msgclient.EmergenciaReaccion
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return &v
	}
	if rec := e.pedir(superv, "supervisor", http.MethodPost, e.porID(e.caso)+"/resolucion",
		msgclient.ResolucionEmergenciaEntrada{VersionEsperada: 2, Nota: "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("Supervisor resuelve: status %d, quiere 403", rec.Code)
	}
	if rec := e.pedir(reaccionB, "reaccion", http.MethodPost, e.porID(e.caso)+"/resolucion",
		msgclient.ResolucionEmergenciaEntrada{VersionEsperada: 2, Nota: "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("B resuelve el de A: status %d, quiere 403; cuerpo %s", rec.Code, rec.Body.String())
	}
	v := resolver(reaccionA, "reaccion")
	if v == nil || v.Id.String() != e.caso || v.Estado != "resuelta" || v.ResueltaEn == nil {
		t.Fatalf("A resuelve: %+v; quiere el caso en resuelta con resuelta_en", v)
	}

	// Notas: de la más antigua a la más reciente, la de resolución al final.
	var notas msgclient.PaginaNotasEmergencia
	quiere("notas como reaccion", get(reaccionB, "reaccion", e.porID(e.caso)+"/notas", &notas), http.StatusOK, "")
	var cuerpos []string
	for _, n := range notas.Items {
		cuerpos = append(cuerpos, n.Cuerpo+"/"+strconv.FormatBool(*n.EsResolucion)+"/"+string(n.AudienciaAutor))
	}
	if want := []string{"sigo en la caseta/false/Cliente", "unidad localizada/true/Interno"}; !slices.Equal(cuerpos, want) {
		t.Fatalf("notas = %v, quiere %v", cuerpos, want)
	}
	quiere("notas como supervisor", get(superv, "supervisor", e.porID(e.caso)+"/notas", nil), http.StatusOK, "")
	quiere("notas como ejecutivo", get(ejec, "ejecutivo", e.porID(e.caso)+"/notas", nil), http.StatusForbidden, "")

	// Cerrar: Reacción no cierra (403); el Monitorista sí y recibe el caso.
	quiere("Reacción cierra", e.pedir(reaccionA, "reaccion", http.MethodPost, e.porID(e.caso)+"/cierre", nil).Code, http.StatusForbidden, "")
	cierre := e.pedir(monit, "monitorista", http.MethodPost, e.porID(e.caso)+"/cierre", nil)
	var cerrado msgclient.CasoEmergencia
	if err := json.Unmarshal(cierre.Body.Bytes(), &cerrado); cierre.Code != http.StatusOK || err != nil || cerrado.Estado != "cerrada" || cerrado.ViajeId.String() != emViaje {
		t.Fatalf("Monitorista cierra: status %d cuerpo %s; quiere 200 con el caso cerrado", cierre.Code, cierre.Body.String())
	}

	// La bandeja ya no trae el caso cerrado.
	var pag msgclient.PaginaBandejaEmergencias
	get(reaccionA, "reaccion", "/api/v1/bandeja/emergencias", &pag)
	if len(pag.Items) != 1 || pag.Items[0].Id != segundo.Id {
		t.Fatalf("bandeja tras cerrar: %+v; quiere solo %s", pag.Items, segundo.Id)
	}
}

// TestParidadPorViajeYPorIdHTTP afirma que cada acción por viaje y su gemela por
// id responden lo mismo al mismo actor, porque corren el mismo caso de uso con la
// misma guarda: denegaciones idénticas, y un ciclo que alterna familias termina
// igual que si usara una sola.
func TestParidadPorViajeYPorIdHTTP(t *testing.T) {
	e := montar(t)
	familias := map[string]string{"por viaje": e.ruta(), "por id": e.porID(e.caso)}
	pedirAmbas := func(paso, sub, rol, metodo, sufijo string, cuerpo any) {
		t.Helper()
		codes := map[string]int{}
		for nombre, base := range familias {
			codes[nombre] = e.pedir(sub, rol, metodo, base+sufijo, cuerpo).Code
		}
		if codes["por viaje"] != codes["por id"] || codes["por id"] != http.StatusForbidden {
			t.Fatalf("%s: %v, quiere 403 en las dos familias", paso, codes)
		}
	}
	pedirAmbas("Supervisor reclama", superv, "supervisor", http.MethodPost, "/reclamos", msgclient.VersionEsperadaEntrada{})
	pedirAmbas("Monitorista reclama", monit, "monitorista", http.MethodPost, "/reclamos", msgclient.VersionEsperadaEntrada{})
	pedirAmbas("Ejecutivo cierra", ejec, "ejecutivo", http.MethodPost, "/cierre", nil)
	pedirAmbas("Reacción cierra", reaccionA, "reaccion", http.MethodPost, "/cierre", nil)
	pedirAmbas("Coordinador resuelve", coord, "coordinador", http.MethodPost, "/resolucion",
		msgclient.ResolucionEmergenciaEntrada{VersionEsperada: 0, Nota: "x"})

	// Ciclo alternando familias: reclama por viaje, transfiere por id, libera por
	// viaje, reclama por id, resuelve por viaje, cierra por id.
	paso := func(nombre string, rec int, want int) {
		t.Helper()
		if rec != want {
			t.Fatalf("%s: status %d, quiere %d", nombre, rec, want)
		}
	}
	paso("A reclama por viaje", e.reclamar(reaccionA, "reaccion", 0).Code, http.StatusCreated)
	paso("A transfiere a B por id", e.pedir(reaccionA, "reaccion", http.MethodPost, e.porID(e.caso)+"/transferencias",
		msgclient.TransferenciaEntrada{ResponsableDestinoId: uuid.MustParse(reaccionB), VersionEsperada: 1}).Code, http.StatusCreated)
	paso("B libera por viaje", e.liberar(t, reaccionB, "reaccion", 2).Code, http.StatusNoContent)
	paso("A reclama por id", e.pedir(reaccionA, "reaccion", http.MethodPost, e.porID(e.caso)+"/reclamos",
		msgclient.VersionEsperadaEntrada{VersionEsperada: 2}).Code, http.StatusCreated)
	paso("A resuelve por viaje", e.pedir(reaccionA, "reaccion", http.MethodPost, e.ruta()+"/resolucion",
		msgclient.ResolucionEmergenciaEntrada{VersionEsperada: 3, Nota: "listo"}).Code, http.StatusOK)
	paso("Coordinador cierra por id", e.pedir(coord, "coordinador", http.MethodPost, e.porID(e.caso)+"/cierre", nil).Code, http.StatusOK)

	var estado string
	var version int64
	if err := e.admin.QueryRow(e.ctx, `SELECT estado, version_reclamo FROM mensajeria.emergencia WHERE id = $1`, e.caso).Scan(&estado, &version); err != nil {
		t.Fatalf("leer caso: %v", err)
	}
	if estado != "cerrada" || version != 3 {
		t.Fatalf("caso al final: %s versión %d, quiere cerrada 3", estado, version)
	}
}
