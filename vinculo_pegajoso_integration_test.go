package mensajeria_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	platformpgx "github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/google/uuid"
)

// TestVinculoPegajosoEjecutivoCliente recorre DEC-128 (DEC-103 #2 y #3) sobre la
// superficie en proceso y HTTP, con dos módulos sobre la MISMA base que solo
// difieren en N: largo (1 h, el dueño siempre disponible) y corto (10 ms, basta
// una pausa de 50 ms para que el dueño deje de estarlo).
//
//  1. El primer reclamo de una conversación del cliente hace dueño al Ejecutivo A.
//  2. La conversación siguiente del cliente le llega solo a A: B no la ve en su
//     bandeja (bootstrap ni catch-up) y su reclamo es 409
//     conflicto_version_reclamo; el Supervisor sí la ve (DEC-131).
//  3. Con A inactivo más de N, B la ve, la reclama y queda como dueño.
//  4. La conversación siguiente le llega solo a B.
//  5. La dirección vencida no revive: B pasa N sin actividad y vuelve; con N
//     largo, A ya ve esa conversación y la reclama, y B sigue de dueño porque
//     está disponible.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestVinculoPegajosoEjecutivoCliente(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		empresa    = "e1000000-0000-0000-0000-000000000001"
		tenantAI27 = "ai27"
		viaje1     = "71000000-0000-0000-0000-000000000001"
		viaje2     = "72000000-0000-0000-0000-000000000002"
		viaje3     = "73000000-0000-0000-0000-000000000003"
		ejecA      = "ea000000-0000-0000-0000-0000000000ea"
		ejecB      = "eb000000-0000-0000-0000-0000000000eb"
		supervisor = "50000000-0000-0000-0000-000000000050"
		persona    = "c1000000-0000-0000-0000-0000000000c1"
	)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	chat := &platformpgx.Pool{Pool: pool}
	sub := &suscriptorCaptura{handlers: map[string]bus.Handler{}}
	largo, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat},
		mensajeria.Deps{Subscriber: sub, VentanaDisponible: time.Hour})
	if err != nil {
		t.Fatalf("construir módulo con N largo: %v", err)
	}
	corto, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat},
		mensajeria.Deps{VentanaDisponible: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("construir módulo con N corto: %v", err)
	}
	apiLargo, apiCorto := largo.Conversaciones(), corto.Conversaciones()
	muxLargo := http.NewServeMux()
	largo.Routes(muxLargo)

	habilitar := func(viaje string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"viaje_id": viaje, "cliente_id": empresa})
		if err := sub.handlers["mensajeria-conversacion-habilitada"](ctx, bus.Sobre{
			EventoID: uuid.NewString(), Tipo: "viaje.mensajeria_habilitada", Version: 1,
			AgregadoID: viaje, OcurridoEn: time.Now().UTC(), Payload: payload,
		}); err != nil {
			t.Fatalf("procesar viaje.mensajeria_habilitada de %s: %v", viaje, err)
		}
	}
	como := func(s, rol string) context.Context {
		tenant, aud := tenantAI27, auth.AudienciaInterno
		if rol == "cliente" {
			tenant, aud = empresa, auth.AudienciaCliente
		}
		return auth.ContextWithClaims(ctx, &auth.Claims{Subject: s, TenantID: tenant, Rol: rol, Audiencia: aud})
	}
	ctxA, ctxB, ctxSup := como(ejecA, "ejecutivo"), como(ejecB, "ejecutivo"), como(supervisor, "supervisor")
	// viajes devuelve los viajes de la bandeja, en orden, y su revisión.
	viajes := func(api mensajeria.ConversacionesAPI, ctxReq context.Context) ([]string, int64) {
		t.Helper()
		tb, err := api.ListarTrabajoBandeja(ctxReq, msgclient.ListarTrabajoMiBandejaParams{})
		if err != nil {
			t.Fatalf("listar bandeja: %v", err)
		}
		vs := []string{}
		for _, it := range tb.Items {
			vs = append(vs, it.ViajeId.String())
		}
		return vs, tb.Revision
	}
	convDe := func(viaje string) msgclient.Id {
		t.Helper()
		c, err := apiLargo.ObtenerConversacionDeViaje(ctxSup, uuid.MustParse(viaje))
		if err != nil {
			t.Fatalf("conversación de %s: %v", viaje, err)
		}
		return c.Id
	}
	reclamar := func(api mensajeria.ConversacionesAPI, ctxReq context.Context, conv msgclient.Id) (*msgclient.Reclamo, error) {
		return api.ReclamarConversacion(ctxReq, conv, msgclient.VersionEsperadaEntrada{VersionEsperada: 0})
	}
	dueno := func() (ejecutivo, conversacion string) {
		t.Helper()
		if err := admin.QueryRow(ctx,
			`SELECT ejecutivo_id::text, conversacion_id::text FROM mensajeria.dueno_cliente
			  WHERE tenant_id = $1 AND cliente_id = $1::uuid`, empresa,
		).Scan(&ejecutivo, &conversacion); err != nil {
			t.Fatalf("leer dueño del cliente: %v", err)
		}
		return ejecutivo, conversacion
	}
	// detalle devuelve el detalle de la auditoría de la última acción dada.
	detalle := func(conv msgclient.Id, actor, accion string) map[string]any {
		t.Helper()
		var d map[string]any
		if err := admin.QueryRow(ctx,
			`SELECT detalle FROM mensajeria.auditoria_mensajeria
			  WHERE conversacion_id = $1 AND actor_real = $2 AND accion = $3
			  ORDER BY id DESC LIMIT 1`, conv, actor, accion,
		).Scan(&d); err != nil {
			t.Fatalf("leer auditoría %s de %s: %v", accion, actor, err)
		}
		return d
	}
	esperarMasQueNCorto := func() { time.Sleep(50 * time.Millisecond) }

	// 1. El primer reclamo de una conversación del cliente hace dueño a A.
	habilitar(viaje1)
	conv1 := convDe(viaje1)
	if vs, _ := viajes(apiLargo, ctxB); !slices.Equal(vs, []string{viaje1}) {
		t.Fatalf("bandeja de B antes de haber dueño: %v; quiere [%s]", vs, viaje1)
	}
	if _, err := reclamar(apiLargo, ctxA, conv1); err != nil {
		t.Fatalf("A reclama la primera conversación: %v", err)
	}
	if e, c := dueno(); e != ejecA || c != conv1.String() {
		t.Fatalf("dueño tras el primer reclamo: %s por %s; quiere %s por %s", e, c, ejecA, conv1)
	}
	if d := detalle(conv1, ejecA, "reclamo.intento_ganado"); d["nuevo_dueno"] != true || d["dueno_anterior"] != nil {
		t.Fatalf("auditoría del primer reclamo: %v; quiere nuevo_dueno sin dueno_anterior", d)
	}

	// 2. La conversación siguiente le llega solo a A.
	habilitar(viaje2)
	conv2 := convDe(viaje2)
	if vs, rev := viajes(apiLargo, ctxA); !slices.Equal(vs, []string{viaje1, viaje2}) || rev != 2 {
		t.Fatalf("bandeja de A: %v revisión %d; quiere [%s %s] y 2", vs, rev, viaje1, viaje2)
	}
	if vs, rev := viajes(apiLargo, ctxB); !slices.Equal(vs, []string{viaje1}) || rev != 2 {
		t.Fatalf("bandeja de B: %v revisión %d; quiere [%s] y 2", vs, rev, viaje1)
	}
	if vs, _ := viajes(apiLargo, ctxSup); !slices.Equal(vs, []string{viaje1, viaje2}) {
		t.Fatalf("bandeja del supervisor: %v; quiere [%s %s]", vs, viaje1, viaje2)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/bandeja/trabajo?desde_revision=1", nil).WithContext(ctxB)
	rec := httptest.NewRecorder()
	muxLargo.ServeHTTP(rec, req)
	var tb msgclient.TrabajoBandeja
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tb) != nil || len(tb.Items) != 0 || tb.Revision != 2 {
		t.Fatalf("catch-up de B desde 1 por HTTP: %d %s; quiere 200 sin items y revisión 2", rec.Code, rec.Body)
	}
	_, err = reclamar(apiLargo, ctxB, conv2)
	if platformerrors.AsCategory(err) != platformerrors.CategoryConflicto || platformerrors.CodigoDe(err) != "conflicto_version_reclamo" {
		t.Fatalf("B reclama la dirigida a A: %v; quiere 409 conflicto_version_reclamo", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/conversaciones/"+conv2.String()+"/reclamos",
		strings.NewReader(`{"version_esperada":0}`)).WithContext(ctxB)
	rec = httptest.NewRecorder()
	muxLargo.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "conflicto_version_reclamo") {
		t.Fatalf("B reclama la dirigida a A por HTTP: %d %s; quiere 409 conflicto_version_reclamo", rec.Code, rec.Body)
	}
	if d := detalle(conv2, ejecB, "reclamo.intento_perdido"); d["dirigida_a"] != ejecA {
		t.Fatalf("auditoría del reclamo perdido de B: %v; quiere dirigida_a %s", d, ejecA)
	}
	// El cliente no reclama (contrato: solo `ejecutivo`); un reclamo lo haría dueño.
	if _, err := reclamar(apiLargo, como(persona, "cliente"), conv2); platformerrors.AsCategory(err) != platformerrors.CategoryProhibido {
		t.Fatalf("el cliente reclama: %v; quiere prohibido", err)
	}

	// 3. A pasa más de N sin actividad: B ve la conversación, la reclama y queda
	// como dueño.
	esperarMasQueNCorto()
	if vs, _ := viajes(apiCorto, ctxB); !slices.Equal(vs, []string{viaje1, viaje2}) {
		t.Fatalf("bandeja de B con A inactivo: %v; quiere [%s %s]", vs, viaje1, viaje2)
	}
	r2, err := reclamar(apiCorto, ctxB, conv2)
	if err != nil || r2.Version != 1 {
		t.Fatalf("B reclama con A inactivo: %v; quiere el reclamo en versión 1", err)
	}
	if e, c := dueno(); e != ejecB || c != conv2.String() {
		t.Fatalf("dueño tras la reasignación: %s por %s; quiere %s por %s", e, c, ejecB, conv2)
	}
	if d := detalle(conv2, ejecB, "reclamo.intento_ganado"); d["nuevo_dueno"] != true || d["dueno_anterior"] != ejecA {
		t.Fatalf("auditoría de la reasignación: %v; quiere nuevo_dueno y dueno_anterior %s", d, ejecA)
	}

	// 4. La conversación siguiente le llega solo a B.
	habilitar(viaje3)
	conv3 := convDe(viaje3)
	if vs, _ := viajes(apiLargo, ctxA); slices.Contains(vs, viaje3) {
		t.Fatalf("bandeja de A: %v; no debe traer %s, dirigida a B", vs, viaje3)
	}
	if vs, _ := viajes(apiLargo, ctxB); !slices.Contains(vs, viaje3) {
		t.Fatalf("bandeja de B: %v; debe traer %s", vs, viaje3)
	}
	if _, err := reclamar(apiLargo, ctxA, conv3); platformerrors.CodigoDe(err) != "conflicto_version_reclamo" {
		t.Fatalf("A reclama la dirigida a B: %v; quiere conflicto_version_reclamo", err)
	}

	// 5. B pasa más de N sin actividad y vuelve (envía en la conversación que
	// tiene). Su dirección sobre la tercera ya venció y no revive: con N largo, B
	// está disponible, pero A ve la conversación y la reclama, y B sigue de dueño.
	esperarMasQueNCorto()
	version := r2.Version
	if _, err := apiCorto.EnviarMensaje(ctxB, conv2, msgclient.EnviarMensajeEntrada{
		ClientMessageId: uuid.New(), Cuerpo: "de vuelta", VersionReclamoEsperada: &version,
	}); err != nil {
		t.Fatalf("B envía al volver: %v", err)
	}
	if vs, _ := viajes(apiLargo, ctxA); !slices.Contains(vs, viaje3) {
		t.Fatalf("bandeja de A tras vencer la dirección: %v; debe traer %s", vs, viaje3)
	}
	if _, err := reclamar(apiLargo, ctxA, conv3); err != nil {
		t.Fatalf("A reclama la conversación con la dirección vencida: %v", err)
	}
	if e, _ := dueno(); e != ejecB {
		t.Fatalf("dueño tras el reclamo de A con B disponible: %s; quiere %s", e, ejecB)
	}
	if d := detalle(conv3, ejecA, "reclamo.intento_ganado"); d["nuevo_dueno"] != nil {
		t.Fatalf("auditoría del reclamo de A con B disponible: %v; quiere sin nuevo_dueno", d)
	}
}
