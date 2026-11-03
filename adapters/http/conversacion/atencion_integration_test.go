package conversacion_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformpgx "github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Actores fijos de las pruebas de E3-T6. ejec y otro son miembros del pool;
// extrano no; admin tiene rol administrador.
const (
	atTenant  = "t1"
	atConv    = "11111111-1111-1111-1111-111111111111"
	atCliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	atBandeja = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	atEjec    = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	atOtro    = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	atExtrano = "abababab-abab-abab-abab-abababababab"
	atAdmin   = "f0f0f0f0-f0f0-f0f0-f0f0-f0f0f0f0f0f0"
)

// escenarioAtencion levanta Postgres con una conversación abierta enrutada a una
// bandeja con dos miembros (ejec, otro) y devuelve el mux del módulo, la conexión
// admin y el pool del rol de app.
type escenarioAtencion struct {
	ctx   context.Context
	admin *pgx.Conn
	pool  *pgxpool.Pool
	mux   *http.ServeMux
	mod   *mensajeria.Module
}

func montarAtencion(t *testing.T) escenarioAtencion {
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
		 VALUES ('`+atConv+`', '`+atTenant+`', gen_random_uuid(), '`+atCliente+`', 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre)
		 VALUES ('`+atBandeja+`', '`+atTenant+`', 'chat', 'cliente A')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa)
		 VALUES ('`+atConv+`', '`+atTenant+`', '`+atBandeja+`', true)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, version, activa)
		 VALUES ('`+atBandeja+`', '`+atEjec+`', '`+atTenant+`', 'ejecutivo', 1, true),
		        ('`+atBandeja+`', '`+atOtro+`', '`+atTenant+`', 'ejecutivo', 1, true)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	t.Cleanup(closePool)
	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{})
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	mux := http.NewServeMux()
	mod.Routes(mux)
	return escenarioAtencion{ctx: ctx, admin: admin, pool: pool, mux: mux, mod: mod}
}

// pedir llama al mux como bff: claims del sujeto en el contexto.
func (e escenarioAtencion) pedir(sub, rol, metodo, ruta string, cuerpo any) *httptest.ResponseRecorder {
	aud := auth.AudienciaInterno
	if rol == "cliente" {
		aud = auth.AudienciaCliente
	}
	var body bytes.Buffer
	if cuerpo != nil {
		_ = json.NewEncoder(&body).Encode(cuerpo)
	}
	req := httptest.NewRequest(metodo, ruta, &body)
	req = req.WithContext(auth.ContextWithClaims(e.ctx, &auth.Claims{Subject: sub, TenantID: atTenant, Rol: rol, Audiencia: aud}))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e escenarioAtencion) entero(t *testing.T, sql string) int64 {
	t.Helper()
	var n int64
	if err := e.admin.QueryRow(e.ctx, sql).Scan(&n); err != nil {
		t.Fatalf("consulta %q: %v", sql, err)
	}
	return n
}

const rutaConv = "/api/v1/conversaciones/" + atConv

// TestMarcarAtendidaHTTP: solo el responsable activo, con la versión vigente,
// marca atendida (DEC-117); enviar una respuesta no la marca.
func TestMarcarAtendidaHTTP(t *testing.T) {
	e := montarAtencion(t)

	if rec := e.pedir(atEjec, "ejecutivo", http.MethodPost, rutaConv+"/reclamos", msgclient.VersionEsperadaEntrada{VersionEsperada: 0}); rec.Code != http.StatusCreated {
		t.Fatalf("reclamar: status %d; cuerpo %s", rec.Code, rec.Body.String())
	}
	v1 := msgclient.VersionReclamo(1)
	if rec := e.pedir(atEjec, "ejecutivo", http.MethodPost, rutaConv+"/mensajes",
		msgclient.EnviarMensajeEntrada{ClientMessageId: uuid.New(), Cuerpo: "respuesta", VersionReclamoEsperada: &v1}); rec.Code != http.StatusCreated {
		t.Fatalf("enviar: status %d; cuerpo %s", rec.Code, rec.Body.String())
	}
	if conv := obtenerConv(t, e); conv.AtendidaEn != nil {
		t.Fatalf("enviar una respuesta no debe marcar atendida (DEC-117), atendida_en = %v", conv.AtendidaEn)
	}

	casos := []struct {
		nombre, sub, rol string
		version          int64
		quiere           int
	}{
		{"miembro no responsable", atOtro, "ejecutivo", 1, http.StatusForbidden},
		{"miembro no responsable con versión obsoleta", atOtro, "ejecutivo", 0, http.StatusConflict},
		{"cliente dueño", atCliente, "cliente", 1, http.StatusForbidden},
		{"sin membresía", atExtrano, "ejecutivo", 1, http.StatusUnauthorized},
		{"responsable con versión obsoleta", atEjec, "ejecutivo", 0, http.StatusConflict},
		{"responsable con versión vigente", atEjec, "ejecutivo", 1, http.StatusNoContent},
	}
	for _, c := range casos {
		rec := e.pedir(c.sub, c.rol, http.MethodPost, rutaConv+"/atendida", msgclient.VersionEsperadaEntrada{VersionEsperada: c.version})
		if rec.Code != c.quiere {
			t.Fatalf("%s: status %d, quiere %d; cuerpo %s", c.nombre, rec.Code, c.quiere, rec.Body.String())
		}
		if c.quiere != http.StatusNoContent && obtenerConv(t, e).AtendidaEn != nil {
			t.Fatalf("%s: el rechazo no debe marcar atendida", c.nombre)
		}
	}
	if conv := obtenerConv(t, e); conv.AtendidaEn == nil {
		t.Fatalf("tras marcar atendida, atendida_en debe estar presente")
	}
	if n := e.entero(t, `SELECT count(*) FROM mensajeria.outbox WHERE tipo = 'conversacion.atendida' AND agregado_id = '`+atConv+`'`); n != 1 {
		t.Fatalf("conversacion.atendida en outbox: %d, quiere 1", n)
	}
}

func obtenerConv(t *testing.T, e escenarioAtencion) msgclient.Conversacion {
	t.Helper()
	rec := e.pedir(atEjec, "ejecutivo", http.MethodGet, rutaConv, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("obtener conversación: status %d; cuerpo %s", rec.Code, rec.Body.String())
	}
	var c msgclient.Conversacion
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("decodificar Conversacion: %v", err)
	}
	return c
}

// TestRecibosP23HTTP: el "leído por AI27" visible al cliente solo lo mueve el
// recibo de lectura del responsable activo (P23). Leer la historia o el recibo de
// un miembro no responsable mueven, a lo más, su cursor personal.
func TestRecibosP23HTTP(t *testing.T) {
	e := montarAtencion(t)

	if rec := e.pedir(atEjec, "ejecutivo", http.MethodPost, rutaConv+"/reclamos", msgclient.VersionEsperadaEntrada{}); rec.Code != http.StatusCreated {
		t.Fatalf("reclamar: status %d; cuerpo %s", rec.Code, rec.Body.String())
	}
	v1 := msgclient.VersionReclamo(1)
	for i := 0; i < 3; i++ {
		if rec := e.pedir(atEjec, "ejecutivo", http.MethodPost, rutaConv+"/mensajes",
			msgclient.EnviarMensajeEntrada{ClientMessageId: uuid.New(), Cuerpo: "m", VersionReclamoEsperada: &v1}); rec.Code != http.StatusCreated {
			t.Fatalf("enviar %d: status %d; cuerpo %s", i, rec.Code, rec.Body.String())
		}
	}

	leidoAI27 := func() int64 {
		return e.entero(t, `SELECT leido_ai27_hasta FROM mensajeria.conversacion WHERE id = '`+atConv+`'`)
	}
	cursor := func(sujeto, col string) int64 {
		return e.entero(t, `SELECT COALESCE((SELECT `+col+` FROM mensajeria.cursor_participante
		                     WHERE conversacion_id = '`+atConv+`' AND participante_id = '`+sujeto+`'), -1)`)
	}
	recibo := func(sub, rol, tipo string, hasta int64) int {
		return e.pedir(sub, rol, http.MethodPost, rutaConv+"/recibos/"+tipo, msgclient.ReciboEntrada{HastaSecuencia: hasta}).Code
	}

	// Vista incidental: leer la historia y la conversación no escribe cursores.
	if rec := e.pedir(atOtro, "ejecutivo", http.MethodGet, rutaConv+"/mensajes", nil); rec.Code != http.StatusOK {
		t.Fatalf("listar mensajes: status %d", rec.Code)
	}
	_ = obtenerConv(t, e)
	if n := e.entero(t, `SELECT count(*) FROM mensajeria.cursor_participante`); n != 0 || leidoAI27() != 0 {
		t.Fatalf("leer la historia movió cursores: %d filas, leído AI27 %d; quiere 0 y 0", n, leidoAI27())
	}

	// Recibo de lectura de un miembro NO responsable y del cliente: su cursor
	// personal avanza (leer implica entregado), el visible al cliente no.
	if c := recibo(atOtro, "ejecutivo", "leido", 3); c != http.StatusNoContent {
		t.Fatalf("recibo leído de otro: status %d", c)
	}
	if c := recibo(atCliente, "cliente", "leido", 3); c != http.StatusNoContent {
		t.Fatalf("recibo leído del cliente: status %d", c)
	}
	if got := [3]int64{cursor(atOtro, "leido_hasta"), cursor(atOtro, "entregado_hasta"), leidoAI27()}; got != [3]int64{3, 3, 0} {
		t.Fatalf("tras vista de no responsable: [leído otro, entregado otro, leído AI27] = %v, quiere [3 3 0]", got)
	}

	// El responsable acusa entregado: tampoco mueve el leído visible.
	if c := recibo(atEjec, "ejecutivo", "entregado", 2); c != http.StatusNoContent {
		t.Fatalf("recibo entregado del responsable: status %d", c)
	}
	if got := [3]int64{cursor(atEjec, "entregado_hasta"), cursor(atEjec, "leido_hasta"), leidoAI27()}; got != [3]int64{2, 0, 0} {
		t.Fatalf("tras entregado del responsable: [entregado, leído, leído AI27] = %v, quiere [2 0 0]", got)
	}

	// Solo el recibo de lectura del responsable activo lo mueve; nunca retrocede
	// y se recorta a la última secuencia (3).
	for _, paso := range []struct{ hasta, quiere int64 }{{2, 2}, {1, 2}, {99, 3}} {
		if c := recibo(atEjec, "ejecutivo", "leido", paso.hasta); c != http.StatusNoContent {
			t.Fatalf("recibo leído %d del responsable: status %d", paso.hasta, c)
		}
		if got := leidoAI27(); got != paso.quiere {
			t.Fatalf("leído AI27 tras recibo %d: %d, quiere %d", paso.hasta, got, paso.quiere)
		}
	}

	// Sin acceso: no autorizado y sin efecto.
	if c := recibo(atExtrano, "ejecutivo", "leido", 3); c != http.StatusUnauthorized {
		t.Fatalf("recibo sin membresía: status %d, quiere 401", c)
	}
	if got := cursor(atExtrano, "leido_hasta"); got != -1 {
		t.Fatalf("recibo rechazado creó cursor: %d", got)
	}

	// Eventos: un recibo.leido por cada avance del cursor personal (otro, cliente,
	// responsable 2 y 3), ninguno por el recibo que no avanzó (1).
	if n := e.entero(t, `SELECT count(*) FROM mensajeria.outbox WHERE tipo = 'recibo.leido' AND agregado_id = '`+atConv+`'`); n != 4 {
		t.Fatalf("recibo.leido en outbox: %d, quiere 4", n)
	}
}

// TestAuditoriaInmutableHTTP: cada intento de reclamo (ganador y perdedor de una
// carrera real), la liberación, la transferencia y la atendida dejan una fila;
// solo administrador las lee; no hay forma de editarlas ni borrarlas.
func TestAuditoriaInmutableHTTP(t *testing.T) {
	e := montarAtencion(t)

	// Carrera del primer reclamo (P19): dos miembros, misma versión esperada 0.
	actores := []string{atEjec, atOtro}
	codigos := make([]int, 2)
	var wg sync.WaitGroup
	inicio := make(chan struct{})
	for i := range actores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-inicio
			codigos[i] = e.pedir(actores[i], "ejecutivo", http.MethodPost, rutaConv+"/reclamos", msgclient.VersionEsperadaEntrada{}).Code
		}(i)
	}
	close(inicio)
	wg.Wait()
	ordenados := append([]int(nil), codigos...)
	sort.Ints(ordenados)
	if ordenados[0] != http.StatusCreated || ordenados[1] != http.StatusConflict {
		t.Fatalf("carrera de reclamo: status %v, quiere un 201 y un 409", codigos)
	}
	ganador, perdedor := actores[0], actores[1]
	if codigos[0] == http.StatusConflict {
		ganador, perdedor = perdedor, ganador
	}

	// Liberación (la conversación queda en v1), nuevo reclamo del perdedor desde
	// el pool (v2), transferencia al ganador (v3), atendida.
	var reclamoID string
	if err := e.admin.QueryRow(e.ctx, `SELECT id FROM mensajeria.reclamo WHERE cerrado_en IS NULL`).Scan(&reclamoID); err != nil {
		t.Fatalf("leer reclamo activo: %v", err)
	}
	pasos := []struct {
		sub, metodo, ruta string
		cuerpo            any
		quiere            int
	}{
		{ganador, http.MethodDelete, rutaConv + "/reclamos/" + reclamoID + "?version_esperada=1", nil, http.StatusNoContent},
		{perdedor, http.MethodPost, rutaConv + "/reclamos", msgclient.VersionEsperadaEntrada{VersionEsperada: 1}, http.StatusCreated},
		{perdedor, http.MethodPost, rutaConv + "/transferencias", msgclient.TransferenciaEntrada{ResponsableDestinoId: uuid.MustParse(ganador), VersionEsperada: 2, Motivo: textoPtr("escalamiento")}, http.StatusCreated},
		{ganador, http.MethodPost, rutaConv + "/atendida", msgclient.VersionEsperadaEntrada{VersionEsperada: 3}, http.StatusNoContent},
	}
	for _, p := range pasos {
		if rec := e.pedir(p.sub, "ejecutivo", p.metodo, p.ruta, p.cuerpo); rec.Code != p.quiere {
			t.Fatalf("%s %s: status %d, quiere %d; cuerpo %s", p.metodo, p.ruta, rec.Code, p.quiere, rec.Body.String())
		}
	}

	rutaAud := "/api/v1/auditoria/conversaciones/" + atConv
	rec := e.pedir(atAdmin, "administrador", http.MethodGet, rutaAud, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("listar auditoría como admin: status %d; cuerpo %s", rec.Code, rec.Body.String())
	}
	var pag msgclient.PaginaAuditoria
	if err := json.Unmarshal(rec.Body.Bytes(), &pag); err != nil {
		t.Fatalf("decodificar PaginaAuditoria: %v", err)
	}
	type fila struct{ accion, actor, motivo string }
	var got []fila
	for _, r := range pag.Items {
		m := ""
		if r.Motivo != nil {
			m = *r.Motivo
		}
		got = append(got, fila{r.Accion, r.ActorReal.String(), m})
	}
	quiere := []fila{
		{"reclamo.intento_ganado", ganador, ""},
		{"reclamo.intento_perdido", perdedor, ""},
		{"reclamo.liberado", ganador, ""},
		{"reclamo.intento_ganado", perdedor, ""},
		{"reclamo.transferido", perdedor, "escalamiento"},
		{"conversacion.atendida", ganador, ""},
	}
	if len(got) != len(quiere) {
		t.Fatalf("auditoría: %d filas %v, quiere %d %v", len(got), got, len(quiere), quiere)
	}
	for i := range quiere {
		if got[i] != quiere[i] {
			t.Fatalf("auditoría fila %d: %v, quiere %v (todas: %v)", i, got[i], quiere[i], got)
		}
	}
	if pag.CursorSiguiente != nil {
		t.Fatalf("una sola página: cursor_siguiente = %q, quiere null", *pag.CursorSiguiente)
	}

	// Solo administrador (otro rol: 403); conversación ajena o inexistente: 404.
	if c := e.pedir(atEjec, "ejecutivo", http.MethodGet, rutaAud, nil).Code; c != http.StatusForbidden {
		t.Fatalf("auditoría como ejecutivo: status %d, quiere 403", c)
	}
	if c := e.pedir(atAdmin, "administrador", http.MethodGet, "/api/v1/auditoria/conversaciones/"+uuid.NewString(), nil).Code; c != http.StatusNotFound {
		t.Fatalf("auditoría de conversación inexistente: status %d, quiere 404", c)
	}

	// Inmutabilidad: no hay ruta que edite o borre auditoría, y el rol de la app
	// no tiene UPDATE ni DELETE sobre la tabla.
	for _, metodo := range []string{http.MethodDelete, http.MethodPatch, http.MethodPut} {
		if c := e.pedir(atAdmin, "administrador", metodo, rutaAud, nil).Code; c != http.StatusMethodNotAllowed {
			t.Fatalf("%s sobre auditoría: status %d, quiere 405", metodo, c)
		}
	}
	for _, sql := range []string{
		`UPDATE mensajeria.auditoria_mensajeria SET accion = 'x'`,
		`DELETE FROM mensajeria.auditoria_mensajeria`,
	} {
		_, err := e.pool.Exec(e.ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%q como svc_mensajeria: err %v, quiere permiso denegado (42501)", sql, err)
		}
	}
	if n := e.entero(t, `SELECT count(*) FROM mensajeria.auditoria_mensajeria`); n != int64(len(quiere)) {
		t.Fatalf("filas de auditoría tras intentos de edición: %d, quiere %d", n, len(quiere))
	}
}

func textoPtr(s string) *string { return &s }

// TestMensajeClienteDesmarcaAtendidaHTTP: un mensaje nuevo del cliente desmarca
// atendida (DEC-122 #1); una respuesta interna no la toca (DEC-117), y el
// reintento idempotente del cliente no es mensaje nuevo.
func TestMensajeClienteDesmarcaAtendidaHTTP(t *testing.T) {
	e := montarAtencion(t)

	if rec := e.pedir(atEjec, "ejecutivo", http.MethodPost, rutaConv+"/reclamos", msgclient.VersionEsperadaEntrada{}); rec.Code != http.StatusCreated {
		t.Fatalf("reclamar: status %d; cuerpo %s", rec.Code, rec.Body.String())
	}
	marcar := func() {
		t.Helper()
		if rec := e.pedir(atEjec, "ejecutivo", http.MethodPost, rutaConv+"/atendida", msgclient.VersionEsperadaEntrada{VersionEsperada: 1}); rec.Code != http.StatusNoContent {
			t.Fatalf("marcar atendida: status %d; cuerpo %s", rec.Code, rec.Body.String())
		}
	}
	enviar := func(sub, rol string, cmid uuid.UUID) {
		t.Helper()
		in := msgclient.EnviarMensajeEntrada{ClientMessageId: cmid, Cuerpo: "m"}
		if rol != "cliente" {
			v1 := msgclient.VersionReclamo(1)
			in.VersionReclamoEsperada = &v1
		}
		if rec := e.pedir(sub, rol, http.MethodPost, rutaConv+"/mensajes", in); rec.Code != http.StatusCreated {
			t.Fatalf("enviar como %s: status %d; cuerpo %s", rol, rec.Code, rec.Body.String())
		}
	}
	atendida := func() bool { return obtenerConv(t, e).AtendidaEn != nil }

	marcar()
	enviar(atEjec, "ejecutivo", uuid.New())
	if !atendida() {
		t.Fatalf("una respuesta interna desmarcó atendida; quiere atendida")
	}

	cmid := uuid.New()
	enviar(atCliente, "cliente", cmid)
	if atendida() {
		t.Fatalf("tras mensaje del cliente: atendida; quiere desmarcada")
	}
	if n := e.entero(t, `SELECT count(*) FROM mensajeria.estado_atendido WHERE conversacion_id = '`+atConv+`' AND atendida`); n != 0 {
		t.Fatalf("filas atendidas tras mensaje del cliente: %d, quiere 0", n)
	}

	marcar()
	enviar(atCliente, "cliente", cmid) // reintento: devuelve el original, no es mensaje nuevo
	if !atendida() {
		t.Fatalf("el reintento idempotente del cliente desmarcó atendida; quiere atendida")
	}
	if n := e.entero(t, `SELECT count(*) FROM mensajeria.mensaje WHERE conversacion_id = '`+atConv+`'`); n != 2 {
		t.Fatalf("mensajes: %d, quiere 2", n)
	}
}
