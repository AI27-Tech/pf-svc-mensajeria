package emergencias_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	platformpgx "github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
)

const (
	emTenant  = "e0000000-0000-0000-0000-0000000000e0" // empresa cliente: tenant == cliente_id (DEC-112)
	emViaje   = "11111111-1111-1111-1111-111111111111"
	clienteU  = "22222222-2222-2222-2222-222222222222" // usuario del portal de la empresa
	reaccionA = "44444444-4444-4444-4444-444444444444"
	reaccionB = "55555555-5555-5555-5555-555555555555"
	superv    = "66666666-6666-6666-6666-666666666666"
	coord     = "77777777-7777-7777-7777-777777777777"
	monit     = "88888888-8888-8888-8888-888888888888"
)

type escenario struct {
	ctx   context.Context
	admin *pgx.Conn
	mux   *http.ServeMux
	mod   *mensajeria.Module
	caso  string
}

// montar no siembra bandejas ni membresías (DEC-130: Reacción es miembro por su
// rol y la bandeja nace con el primer caso). Siembra la conversación del viaje,
// que es como mensajeria conoce su empresa (DEC-133). El cliente de la empresa
// activa un caso, que queda bajo el tenant de la empresa; los internos llegan con
// el tenant de AI27 (DEC-129).
func montar(t *testing.T) escenario {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	t.Cleanup(closeAdmin)
	dbtest.Reiniciar(t, ctx, admin)
	t.Cleanup(func() { dbtest.Reiniciar(t, ctx, admin) })
	dbtest.AplicarMigraciones(t, ctx, admin)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	t.Cleanup(closePool)
	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{})
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	mux := http.NewServeMux()
	mod.Routes(mux)
	e := escenario{ctx: ctx, admin: admin, mux: mux, mod: mod}
	dbtest.Exec(t, ctx, admin, `INSERT INTO mensajeria.conversacion (tenant_id, viaje_id, cliente_id)
		 VALUES ('`+emTenant+`', '`+emViaje+`', '`+emTenant+`')`)

	rec := e.pedir(clienteU, "cliente", http.MethodPost, "/api/v1/viajes/"+emViaje+"/emergencias",
		msgclient.ActivarEmergenciaEntrada{ActivacionId: uuid.New()})
	var caso msgclient.CasoEmergencia
	if err := json.Unmarshal(rec.Body.Bytes(), &caso); rec.Code != http.StatusCreated || err != nil {
		t.Fatalf("activar: status %d, cuerpo %s", rec.Code, rec.Body.String())
	}
	e.caso = caso.Id.String()
	return e
}

// claims arma el token: el cliente con el tenant de su empresa, el interno con
// el de AI27 (DEC-129).
func claims(sub, rol string) *auth.Claims {
	if rol == "cliente" {
		return &auth.Claims{Subject: sub, TenantID: emTenant, Rol: rol, Audiencia: auth.AudienciaCliente}
	}
	return &auth.Claims{Subject: sub, TenantID: "ai27", Rol: rol, Audiencia: auth.AudienciaInterno}
}

// pedir llama al mux como bff: claims del sujeto en el contexto.
func (e escenario) pedir(sub, rol, metodo, ruta string, cuerpo any) *httptest.ResponseRecorder {
	var body bytes.Buffer
	if cuerpo != nil {
		_ = json.NewEncoder(&body).Encode(cuerpo)
	}
	req := httptest.NewRequest(metodo, ruta, &body)
	req = req.WithContext(auth.ContextWithClaims(e.ctx, claims(sub, rol)))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e escenario) ruta() string { return "/api/v1/viajes/" + emViaje + "/emergencias/" + e.caso }

// pedirComo llama al mux como un usuario cliente de la empresa (tenant) dada.
func (e escenario) pedirComo(sub, tenant, metodo, ruta string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(metodo, ruta, nil)
	req = req.WithContext(auth.ContextWithClaims(e.ctx,
		&auth.Claims{Subject: sub, TenantID: tenant, Rol: "cliente", Audiencia: auth.AudienciaCliente}))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e escenario) reclamar(sub, rol string, v int64) *httptest.ResponseRecorder {
	return e.pedir(sub, rol, http.MethodPost, e.ruta()+"/reclamos", msgclient.VersionEsperadaEntrada{VersionEsperada: v})
}

func (e escenario) liberar(t *testing.T, sub, rol string, v int64) *httptest.ResponseRecorder {
	rid, _, _ := e.activo(t)
	return e.pedir(sub, rol, http.MethodDelete, e.ruta()+"/reclamos/"+rid+"?version_esperada="+strconv.FormatInt(v, 10), nil)
}

func (e escenario) transferir(sub, rol, destino string, v int64) *httptest.ResponseRecorder {
	return e.pedir(sub, rol, http.MethodPost, e.ruta()+"/transferencias",
		msgclient.TransferenciaEntrada{ResponsableDestinoId: uuid.MustParse(destino), VersionEsperada: v})
}

// activo devuelve el reclamo activo: id, responsable y versión ("" si no hay).
func (e escenario) activo(t *testing.T) (string, string, int64) {
	t.Helper()
	var rid, actor string
	var v int64
	err := e.admin.QueryRow(e.ctx,
		`SELECT id::text, actor_real::text, version FROM mensajeria.reclamo_emergencia WHERE cerrado_en IS NULL`).Scan(&rid, &actor, &v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", 0
	}
	if err != nil {
		t.Fatalf("leer reclamo activo: %v", err)
	}
	return rid, actor, v
}

func codigo(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e msgclient.Error
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Codigo
}

// TestReclamarCasoConResponsableHTTP cubre DEC-125: con reclamo activo, otro
// miembro de Reacción con la versión vigente recibe 409 por HTTP y por la
// superficie en proceso, el caso sigue con su responsable y cada intento queda
// auditado. Se salta sin MENSAJERIA_TEST_DB.
func TestReclamarCasoConResponsableHTTP(t *testing.T) {
	e := montar(t)

	if rec := e.reclamar(reaccionA, "reaccion", 0); rec.Code != http.StatusCreated {
		t.Fatalf("A reclama del pool: status %d, quiere 201; cuerpo %s", rec.Code, rec.Body.String())
	}
	if rec := e.reclamar(reaccionB, "reaccion", 1); rec.Code != http.StatusConflict || codigo(t, rec) != "conflicto" {
		t.Fatalf("B reclama el de A: status %d codigo %q, quiere 409 conflicto", rec.Code, codigo(t, rec))
	}
	ctxB := auth.ContextWithClaims(e.ctx, claims(reaccionB, "reaccion"))
	if _, err := e.mod.Emergencias().ReclamarEmergencia(ctxB, uuid.MustParse(emViaje), uuid.MustParse(e.caso),
		msgclient.VersionEsperadaEntrada{VersionEsperada: 1}); platformerrors.AsCategory(err) != platformerrors.CategoryConflicto {
		t.Fatalf("B reclama el de A en proceso: err %v, quiere conflicto", err)
	}
	if _, actor, v := e.activo(t); actor != reaccionA || v != 1 {
		t.Fatalf("tras los 409: responsable %s versión %d, quiere %s 1", actor, v, reaccionA)
	}

	rows, err := e.admin.Query(e.ctx,
		`SELECT accion, actor_real::text, detalle->>'caso_emergencia_id',
		        detalle->>'version_esperada', COALESCE(detalle->>'version_actual', detalle->>'version_reclamo'),
		        conversacion_id IS NULL
		   FROM mensajeria.auditoria_mensajeria ORDER BY id`)
	if err != nil {
		t.Fatalf("leer auditoría: %v", err)
	}
	type fila struct {
		accion, actor, caso, esperada, version string
		sinConv                                bool
	}
	var got []fila
	for rows.Next() {
		var f fila
		if err := rows.Scan(&f.accion, &f.actor, &f.caso, &f.esperada, &f.version, &f.sinConv); err != nil {
			t.Fatalf("escanear auditoría: %v", err)
		}
		got = append(got, f)
	}
	want := []fila{
		{"emergencia.reclamo.intento_ganado", reaccionA, e.caso, "0", "1", true},
		{"emergencia.reclamo.intento_perdido", reaccionB, e.caso, "1", "1", true},
		{"emergencia.reclamo.intento_perdido", reaccionB, e.caso, "1", "1", true},
	}
	if len(got) != len(want) {
		t.Fatalf("auditoría: %d filas %+v, quiere %+v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("auditoría[%d]: %+v, quiere %+v", i, got[i], want[i])
		}
	}
}

// TestLiberarTransferirCasoAjenoHTTP cubre DEC-126: otro miembro de Reacción no
// libera ni transfiere el caso de A (403 por HTTP y en proceso); un Monitorista
// recibe 403 (DEC-132); Supervisor y Coordinador, sin fila de membresía,
// liberan y transfieren el caso ajeno; el responsable libera y transfiere el
// suyo. Se salta sin MENSAJERIA_TEST_DB.
func TestLiberarTransferirCasoAjenoHTTP(t *testing.T) {
	e := montar(t)
	api := e.mod.Emergencias()
	viaje, caso := uuid.MustParse(emViaje), uuid.MustParse(e.caso)
	enProceso := func(sub, rol string) context.Context { return auth.ContextWithClaims(e.ctx, claims(sub, rol)) }
	quiere := func(paso string, rec *httptest.ResponseRecorder, status int) {
		t.Helper()
		if rec.Code != status {
			t.Fatalf("%s: status %d, quiere %d; cuerpo %s", paso, rec.Code, status, rec.Body.String())
		}
	}
	responsable := func(paso, actor string, version int64) {
		t.Helper()
		if _, a, v := e.activo(t); a != actor || v != version {
			t.Fatalf("%s: responsable %q versión %d, quiere %q %d", paso, a, v, actor, version)
		}
	}

	quiere("A reclama", e.reclamar(reaccionA, "reaccion", 0), http.StatusCreated)

	// B, de Reacción, no es el responsable: 403 prohibido por HTTP y en proceso.
	rec := e.liberar(t, reaccionB, "reaccion", 1)
	if rec.Code != http.StatusForbidden || codigo(t, rec) != "prohibido" {
		t.Fatalf("B libera el de A: status %d codigo %q, quiere 403 prohibido", rec.Code, codigo(t, rec))
	}
	rec = e.transferir(reaccionB, "reaccion", reaccionB, 1)
	if rec.Code != http.StatusForbidden || codigo(t, rec) != "prohibido" {
		t.Fatalf("B transfiere el de A: status %d codigo %q, quiere 403 prohibido", rec.Code, codigo(t, rec))
	}
	rid, _, _ := e.activo(t)
	if err := api.LiberarReclamoEmergencia(enProceso(reaccionB, "reaccion"), viaje, caso, uuid.MustParse(rid), 1); platformerrors.AsCategory(err) != platformerrors.CategoryProhibido {
		t.Fatalf("B libera el de A en proceso: err %v, quiere prohibido", err)
	}
	if _, err := api.TransferirEmergencia(enProceso(reaccionB, "reaccion"), viaje, caso,
		msgclient.TransferenciaEntrada{ResponsableDestinoId: uuid.MustParse(reaccionB), VersionEsperada: 1}); platformerrors.AsCategory(err) != platformerrors.CategoryProhibido {
		t.Fatalf("B transfiere el de A en proceso: err %v, quiere prohibido", err)
	}
	// Sin rol de Reacción ni mando: 403 sobre un caso que existe (DEC-132).
	quiere("Monitorista libera", e.liberar(t, monit, "monitorista", 1), http.StatusForbidden)
	responsable("tras los rechazos", reaccionA, 1)

	// Supervisor transfiere el de A a B (HTTP); Coordinador libera el de B (en proceso).
	quiere("Supervisor transfiere", e.transferir(superv, "supervisor", reaccionB, 1), http.StatusCreated)
	responsable("tras Supervisor transfiere", reaccionB, 2)
	rid, _, _ = e.activo(t)
	if err := api.LiberarReclamoEmergencia(enProceso(coord, "coordinador"), viaje, caso, uuid.MustParse(rid), 2); err != nil {
		t.Fatalf("Coordinador libera en proceso: %v", err)
	}
	responsable("tras Coordinador libera", "", 0)

	// A reclama; Supervisor libera (en proceso); A reclama; Coordinador transfiere a B (HTTP).
	quiere("A reclama de nuevo", e.reclamar(reaccionA, "reaccion", 2), http.StatusCreated)
	rid, _, _ = e.activo(t)
	if err := api.LiberarReclamoEmergencia(enProceso(superv, "supervisor"), viaje, caso, uuid.MustParse(rid), 3); err != nil {
		t.Fatalf("Supervisor libera en proceso: %v", err)
	}
	quiere("A reclama otra vez", e.reclamar(reaccionA, "reaccion", 3), http.StatusCreated)
	quiere("Coordinador transfiere", e.transferir(coord, "coordinador", reaccionB, 4), http.StatusCreated)
	responsable("tras Coordinador transfiere", reaccionB, 5)

	// El responsable transfiere y libera el suyo.
	quiere("B transfiere el suyo", e.transferir(reaccionB, "reaccion", reaccionA, 5), http.StatusCreated)
	responsable("tras B transfiere", reaccionA, 6)
	quiere("A libera el suyo", e.liberar(t, reaccionA, "reaccion", 6), http.StatusNoContent)
	responsable("tras A libera", "", 0)

	var estado string
	var version int64
	if err := e.admin.QueryRow(e.ctx, `SELECT estado, version_reclamo FROM mensajeria.emergencia`).Scan(&estado, &version); err != nil {
		t.Fatalf("leer caso: %v", err)
	}
	if estado != "abierta" || version != 6 {
		t.Fatalf("caso al final: %s versión %d, quiere abierta 6", estado, version)
	}
}

// TestCasoDeMonitoreoBajoLaEmpresaDelViajeHTTP cubre DEC-133 de punta a punta:
//   - un Monitorista (tenant de AI27) activa sobre el viaje de la empresa: 201 y
//     el caso queda bajo el tenant y cliente_id de la empresa, no de AI27;
//   - el cliente de la empresa lista los dos casos (el suyo y el de Monitoreo) en
//     la vista recortada: sin origen, actor_real ni reclamo_activo;
//   - el cliente de otra empresa lista vacío y, si activa sobre ese viaje, 404
//     sin crear caso; un viaje que mensajeria no conoce, 404;
//   - Reacción ve los dos casos completos (DEC-129).
//
// Se salta sin MENSAJERIA_TEST_DB.
func TestCasoDeMonitoreoBajoLaEmpresaDelViajeHTTP(t *testing.T) {
	e := montar(t)
	activar := func(sub, rol, viaje string) *httptest.ResponseRecorder {
		return e.pedir(sub, rol, http.MethodPost, "/api/v1/viajes/"+viaje+"/emergencias",
			msgclient.ActivarEmergenciaEntrada{ActivacionId: uuid.New()})
	}
	listar := func(rec *httptest.ResponseRecorder) []msgclient.CasoEmergencia {
		t.Helper()
		var pag msgclient.PaginaEmergencias
		if err := json.Unmarshal(rec.Body.Bytes(), &pag); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("listar: status %d cuerpo %s", rec.Code, rec.Body.String())
		}
		return pag.Items
	}
	rutaViaje := "/api/v1/viajes/" + emViaje + "/emergencias"

	rec := activar(monit, "monitorista", emViaje)
	var porMonitoreo msgclient.CasoEmergencia
	if err := json.Unmarshal(rec.Body.Bytes(), &porMonitoreo); rec.Code != http.StatusCreated || err != nil {
		t.Fatalf("Monitorista activa: status %d cuerpo %s", rec.Code, rec.Body.String())
	}
	// La activación devuelve el caso completo, no solo su id (contrato).
	if porMonitoreo.ViajeId.String() != emViaje || porMonitoreo.Estado != "abierta" ||
		porMonitoreo.ActivadaEn.IsZero() || porMonitoreo.Origen != "operacion" {
		t.Fatalf("respuesta de activar: %+v; quiere viaje %s, abierta, activada_en y origen operacion", porMonitoreo, emViaje)
	}
	var tenant, cliente, origen string
	if err := e.admin.QueryRow(e.ctx,
		`SELECT tenant_id, cliente_id::text, origen FROM mensajeria.emergencia WHERE id = $1`,
		porMonitoreo.Id).Scan(&tenant, &cliente, &origen); err != nil {
		t.Fatalf("leer caso de Monitoreo: %v", err)
	}
	if tenant != emTenant || cliente != emTenant || origen != "operacion" {
		t.Fatalf("caso de Monitoreo: tenant %s cliente %s origen %s; quiere %s %s operacion", tenant, cliente, origen, emTenant, emTenant)
	}

	// El cliente de la empresa ve los dos casos, recortados.
	crudo := e.pedir(clienteU, "cliente", http.MethodGet, rutaViaje, nil)
	items := listar(crudo)
	if len(items) != 2 || items[0].Id.String() != e.caso || items[1].Id != porMonitoreo.Id {
		t.Fatalf("cliente de la empresa: %+v; quiere [%s %s]", items, e.caso, porMonitoreo.Id)
	}
	for _, c := range items {
		if c.ClienteId.String() != emTenant || c.ViajeId.String() != emViaje || c.Estado != "abierta" ||
			c.Origen != "" || c.ActorReal != nil || c.ReclamoActivo != nil {
			t.Fatalf("vista del cliente: %+v; quiere su empresa y viaje, abierta, sin origen, actor_real ni reclamo", c)
		}
	}
	if body := crudo.Body.String(); strings.Contains(body, monit) || strings.Contains(body, clienteU) || strings.Contains(body, "operacion") {
		t.Fatalf("la vista del cliente no revela quién activó ni el origen: %s", body)
	}

	// El cliente de otra empresa no ve los casos ni activa sobre el viaje.
	if items := listar(e.pedirComo(clienteU, otraEmp, http.MethodGet, rutaViaje)); len(items) != 0 {
		t.Fatalf("cliente de otra empresa: %+v; quiere vacío", items)
	}
	ctxOtra := auth.ContextWithClaims(e.ctx,
		&auth.Claims{Subject: clienteU, TenantID: otraEmp, Rol: "cliente", Audiencia: auth.AudienciaCliente})
	if _, _, err := e.mod.Emergencias().ActivarEmergencia(ctxOtra, uuid.MustParse(emViaje),
		msgclient.ActivarEmergenciaEntrada{ActivacionId: uuid.New()}); platformerrors.AsCategory(err) != platformerrors.CategoryNoEncontrado {
		t.Fatalf("cliente de otra empresa activa: err %v, quiere no_encontrado", err)
	}
	if rec := activar(monit, "monitorista", noExiste); rec.Code != http.StatusNotFound {
		t.Fatalf("Monitorista activa sobre un viaje desconocido: status %d, quiere 404; cuerpo %s", rec.Code, rec.Body.String())
	}
	// Reacción, Ejecutivo y Administrador no activan (DEC-045 #9): 403 sin crear caso.
	for _, rol := range []string{"reaccion", "ejecutivo", "administrador", "jefe_operativo"} {
		if rec := activar(uuid.New().String(), rol, emViaje); rec.Code != http.StatusForbidden {
			t.Fatalf("%s activa: status %d, quiere 403; cuerpo %s", rol, rec.Code, rec.Body.String())
		}
	}
	var casos int
	if err := e.admin.QueryRow(e.ctx, `SELECT count(*) FROM mensajeria.emergencia`).Scan(&casos); err != nil || casos != 2 {
		t.Fatalf("casos = %d (err %v), quiere 2: los rechazos no crean nada", casos, err)
	}

	// El monitoreo y el Administrador ven los dos casos completos, sin acotar por
	// tenant (DEC-129, DEC-135). Reacción y Ejecutivo no listan por viaje: 403.
	for _, rol := range []string{"supervisor", "administrador"} {
		items = listar(e.pedir(uuid.New().String(), rol, http.MethodGet, rutaViaje, nil))
		if len(items) != 2 || items[0].Origen != "cliente" || items[1].Origen != "operacion" ||
			items[1].ActorReal == nil || items[1].ActorReal.String() != monit {
			t.Fatalf("%s: %+v; quiere los dos casos con origen cliente/operacion y el actor de Monitoreo", rol, items)
		}
	}
	for _, rol := range []string{"reaccion", "ejecutivo"} {
		if rec := e.pedir(uuid.New().String(), rol, http.MethodGet, rutaViaje, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("%s lista por viaje: status %d, quiere 403; cuerpo %s", rol, rec.Code, rec.Body.String())
		}
	}

	// Notas: anotan el monitoreo y el Administrador; Reacción y Ejecutivo no
	// (DEC-121 #4, DEC-135).
	rutaNotas := rutaViaje + "/" + porMonitoreo.Id.String() + "/notas"
	nota := msgclient.NotaEmergenciaEntrada{Cuerpo: "unidad detenida en caseta"}
	for rol, want := range map[string]int{
		"monitorista": http.StatusCreated, "administrador": http.StatusCreated,
		"reaccion": http.StatusForbidden, "ejecutivo": http.StatusForbidden,
	} {
		if rec := e.pedir(uuid.New().String(), rol, http.MethodPost, rutaNotas, nota); rec.Code != want {
			t.Fatalf("nota como %s: status %d, quiere %d; cuerpo %s", rol, rec.Code, want, rec.Body.String())
		}
	}
	var notas int
	if err := e.admin.QueryRow(e.ctx, `SELECT count(*) FROM mensajeria.nota_emergencia WHERE emergencia_id = $1`,
		porMonitoreo.Id).Scan(&notas); err != nil || notas != 2 {
		t.Fatalf("notas = %d (err %v), quiere 2: los rechazos no escriben", notas, err)
	}
}
