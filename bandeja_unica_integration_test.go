package mensajeria_test

import (
	"context"
	"encoding/json"
	"io"
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
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/google/uuid"
)

// suscriptorCaptura implementa bus.Subscriber guardando el handler de cada
// consumidor durable, para entregarle un evento como lo haría el broker.
type suscriptorCaptura struct{ handlers map[string]bus.Handler }

func (s *suscriptorCaptura) Subscribe(durable string, _ bus.Sensibilidad, _ []string, h bus.Handler) (worker.Worker, error) {
	s.handlers[durable] = h
	return workerNulo{}, nil
}

type workerNulo struct{}

func (workerNulo) Name() string                    { return "nulo" }
func (workerNulo) Ready(ctx context.Context) error { return nil }
func (workerNulo) Run(ctx context.Context) error   { <-ctx.Done(); return nil }

// TestBandejaUnicaDesdeElEvento recorre el camino del smoke de core (DEC-124,
// DEC-127, DEC-129, DEC-131, DEC-132) sin sembrar bandejas, rutas ni
// membresías: viaje.mensajeria_habilitada crea la conversación por el consumidor
// del módulo; los internos llegan con el tenant de AI27, no el del cliente; la
// bandeja del Ejecutivo trae las conversaciones de DOS empresas con una revisión
// coherente (en proceso y HTTP); el Ejecutivo la reclama; una persona de la
// empresa cliente (sub ≠ cliente_id) la abre por viaje y escribe, y no ve la de
// la otra empresa; un monitorista no tiene bandeja y recibe 403 en la
// conversación; un supervisor lee bandeja, conversación y mensajes pero no envía
// ni reclama (403).
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestBandejaUnicaDesdeElEvento(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		empresa     = "e1000000-0000-0000-0000-000000000001" // cliente_id == tenant (DEC-112)
		empresa2    = "e2000000-0000-0000-0000-000000000002"
		tenantAI27  = "ai27" // tenant de los usuarios internos (DEC-129)
		viaje1      = "71000000-0000-0000-0000-000000000001"
		viaje2      = "72000000-0000-0000-0000-000000000002"
		viaje3      = "73000000-0000-0000-0000-000000000003" // de empresa2
		supervisor  = "50000000-0000-0000-0000-000000000050"
		persona     = "c1000000-0000-0000-0000-0000000000c1" // usuario del portal; sub ≠ cliente_id
		ejecutivo   = "ec000000-0000-0000-0000-0000000000ec"
		monitorista = "30000000-0000-0000-0000-000000000030"
	)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	chat := &platformpgx.Pool{Pool: pool}
	sub := &suscriptorCaptura{handlers: map[string]bus.Handler{}}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{Subscriber: sub})
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	api := mod.Conversaciones()
	mux := http.NewServeMux()
	mod.Routes(mux)

	habilitar := func(viaje string, cliente ...string) {
		t.Helper()
		cli := empresa
		if len(cliente) > 0 {
			cli = cliente[0]
		}
		payload, _ := json.Marshal(map[string]string{"viaje_id": viaje, "cliente_id": cli})
		h := sub.handlers["mensajeria-conversacion-habilitada"]
		if h == nil {
			t.Fatal("el módulo no suscribió el consumidor de viaje.mensajeria_habilitada")
		}
		if err := h(ctx, bus.Sobre{
			EventoID: uuid.NewString(), Tipo: "viaje.mensajeria_habilitada", Version: 1,
			AgregadoID: viaje, OcurridoEn: time.Now().UTC(), Payload: payload,
		}); err != nil {
			t.Fatalf("procesar viaje.mensajeria_habilitada: %v", err)
		}
	}
	como := func(s, rol string, aud auth.Audiencia) context.Context {
		tenant := tenantAI27
		if aud == auth.AudienciaCliente {
			tenant = empresa
		}
		return auth.ContextWithClaims(ctx, &auth.Claims{Subject: s, TenantID: tenant, Rol: rol, Audiencia: aud})
	}
	ctxEjec := como(ejecutivo, "ejecutivo", auth.AudienciaInterno)
	ctxMon := como(monitorista, "monitorista", auth.AudienciaInterno)
	ctxSup := como(supervisor, "supervisor", auth.AudienciaInterno)
	ctxPersona := como(persona, "cliente", auth.AudienciaCliente)
	statusHTTP := func(ctxReq context.Context, metodo, ruta string, cuerpo ...string) (int, []byte) {
		var b io.Reader
		if len(cuerpo) > 0 {
			b = strings.NewReader(cuerpo[0])
		}
		req := httptest.NewRequest(metodo, ruta, b).WithContext(ctxReq)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	categoria := platformerrors.AsCategory

	// 1. El evento crea la conversación; la reentrega no la duplica ni la reenruta.
	habilitar(viaje1)
	habilitar(viaje1)

	tb, err := api.ListarTrabajoBandeja(ctxEjec, msgclient.ListarTrabajoMiBandejaParams{})
	if err != nil {
		t.Fatalf("bandeja del ejecutivo: %v", err)
	}
	if tb.Revision != 1 || len(tb.Items) != 1 || tb.Items[0].ViajeId.String() != viaje1 {
		t.Fatalf("bandeja: revision %d, %d items; quiere revision 1 y la conversación de %s", tb.Revision, len(tb.Items), viaje1)
	}
	conv := tb.Items[0]
	if conv.ClienteId.String() != empresa || conv.BandejaId == nil {
		t.Fatalf("conversación: cliente %s bandeja %v; quiere cliente %s y bandeja asignada", conv.ClienteId, conv.BandejaId, empresa)
	}
	code, body := statusHTTP(ctxEjec, http.MethodGet, "/api/v1/bandeja/trabajo")
	var tbHTTP msgclient.TrabajoBandeja
	if code != http.StatusOK || json.Unmarshal(body, &tbHTTP) != nil || len(tbHTTP.Items) != 1 || tbHTTP.Items[0].Id != conv.Id {
		t.Fatalf("bandeja por HTTP: status %d cuerpo %s; quiere 200 con %s", code, body, conv.Id)
	}

	// 2. Un segundo viaje de la empresa entra a la MISMA bandeja, revisión 2.
	habilitar(viaje2)
	tb, err = api.ListarTrabajoBandeja(ctxEjec, msgclient.ListarTrabajoMiBandejaParams{})
	if err != nil {
		t.Fatalf("bandeja tras el segundo viaje: %v", err)
	}
	if tb.Revision != 2 || len(tb.Items) != 2 || *tb.Items[1].BandejaId != *conv.BandejaId {
		t.Fatalf("segundo viaje: revision %d, %d items; quiere revision 2, 2 items en la misma bandeja", tb.Revision, len(tb.Items))
	}

	// 2b. Un viaje de OTRA empresa entra a la bandeja de su tenant, y el Ejecutivo
	// (tenant de AI27) lo ve junto a los de la primera: revisión 3, la siguiente
	// de todas las bandejas de chat. El catch-up desde 2 trae solo esa alta.
	habilitar(viaje3, empresa2)
	tb, err = api.ListarTrabajoBandeja(ctxEjec, msgclient.ListarTrabajoMiBandejaParams{})
	if err != nil {
		t.Fatalf("bandeja con dos empresas: %v", err)
	}
	clientes := []string{}
	for _, it := range tb.Items {
		clientes = append(clientes, it.ClienteId.String())
	}
	if tb.Revision != 3 || !slices.Equal(clientes, []string{empresa, empresa, empresa2}) {
		t.Fatalf("bandeja con dos empresas: revision %d clientes %v; quiere 3 y [%s %s %s]", tb.Revision, clientes, empresa, empresa, empresa2)
	}
	convB := tb.Items[2]
	code, body = statusHTTP(ctxEjec, http.MethodGet, "/api/v1/bandeja/trabajo?desde_revision=2")
	tbHTTP = msgclient.TrabajoBandeja{}
	if code != http.StatusOK || json.Unmarshal(body, &tbHTTP) != nil || tbHTTP.Revision != 3 ||
		len(tbHTTP.Items) != 1 || tbHTTP.Items[0].Id != convB.Id {
		t.Fatalf("catch-up desde 2 por HTTP: status %d cuerpo %s; quiere 200, revisión 3 y solo %s", code, body, convB.Id)
	}

	// 3. El Ejecutivo la reclama desde el pool sin fila de membresía.
	r, err := api.ReclamarConversacion(ctxEjec, conv.Id, msgclient.VersionEsperadaEntrada{VersionEsperada: 0})
	if err != nil {
		t.Fatalf("reclamar como ejecutivo: %v", err)
	}
	if r.Version != 1 || r.ResponsableId.String() != ejecutivo {
		t.Fatalf("reclamo: version %d responsable %s; quiere 1 y %s", r.Version, r.ResponsableId, ejecutivo)
	}

	// 4. La persona de la empresa abre la conversación por viaje y escribe.
	c, err := api.ObtenerConversacionDeViaje(ctxPersona, uuid.MustParse(viaje1))
	if err != nil {
		t.Fatalf("conversación del viaje como persona de la empresa: %v", err)
	}
	if c.Id != conv.Id {
		t.Fatalf("conversación del viaje = %s, quiere %s", c.Id, conv.Id)
	}
	m, err := api.EnviarMensaje(ctxPersona, conv.Id, msgclient.EnviarMensajeEntrada{ClientMessageId: uuid.New(), Cuerpo: "hola"})
	if err != nil {
		t.Fatalf("enviar como persona de la empresa: %v", err)
	}
	if m.Secuencia != 1 {
		t.Fatalf("secuencia del mensaje = %d, quiere 1", m.Secuencia)
	}
	if code, body := statusHTTP(ctxPersona, http.MethodGet, "/api/v1/viajes/"+viaje1+"/conversacion"); code != http.StatusOK {
		t.Fatalf("conversación del viaje por HTTP: status %d cuerpo %s; quiere 200", code, body)
	}
	// La persona de la primera empresa no ve la conversación de la otra: 404.
	if _, err := api.ObtenerConversacionDeViaje(ctxPersona, uuid.MustParse(viaje3)); categoria(err) != platformerrors.CategoryNoEncontrado {
		t.Fatalf("conversación de otra empresa por viaje: categoría %v, quiere no_encontrado", categoria(err))
	}
	if code, body := statusHTTP(ctxPersona, http.MethodGet, "/api/v1/conversaciones/"+convB.Id.String()); code != http.StatusNotFound {
		t.Fatalf("conversación de otra empresa por HTTP: status %d cuerpo %s; quiere 404", code, body)
	}

	// 5. Un monitorista no tiene bandeja (404) y no lee la conversación (403).
	if _, err := api.ListarTrabajoBandeja(ctxMon, msgclient.ListarTrabajoMiBandejaParams{}); categoria(err) != platformerrors.CategoryNoEncontrado {
		t.Fatalf("bandeja de monitorista: categoría %v, quiere no_encontrado", categoria(err))
	}
	if _, err := api.ObtenerConversacion(ctxMon, conv.Id); categoria(err) != platformerrors.CategoryProhibido {
		t.Fatalf("conversación como monitorista: categoría %v, quiere prohibido", categoria(err))
	}

	// 6. Un supervisor lee bandeja, conversación y mensajes, y no escribe (DEC-131).
	if tb, err := api.ListarTrabajoBandeja(ctxSup, msgclient.ListarTrabajoMiBandejaParams{}); err != nil || tb.Revision != 3 || len(tb.Items) != 3 {
		t.Fatalf("bandeja del supervisor: %v; quiere revisión 3 y 3 conversaciones", err)
	}
	if c, err := api.ObtenerConversacion(ctxSup, conv.Id); err != nil || c.Id != conv.Id {
		t.Fatalf("conversación como supervisor: %v", err)
	}
	if pm, err := api.ListarMensajes(ctxSup, conv.Id, msgclient.ListarMensajesParams{}); err != nil || len(pm.Items) != 1 || pm.Items[0].Cuerpo != "hola" {
		t.Fatalf("mensajes como supervisor: %v; quiere el mensaje \"hola\"", err)
	}
	if _, err := api.EnviarMensaje(ctxSup, conv.Id, msgclient.EnviarMensajeEntrada{ClientMessageId: uuid.New(), Cuerpo: "x"}); categoria(err) != platformerrors.CategoryProhibido {
		t.Fatalf("enviar como supervisor: categoría %v, quiere prohibido", categoria(err))
	}
	if _, err := api.ReclamarConversacion(ctxSup, convB.Id, msgclient.VersionEsperadaEntrada{}); categoria(err) != platformerrors.CategoryProhibido {
		t.Fatalf("reclamar como supervisor: categoría %v, quiere prohibido", categoria(err))
	}
	cuerpo := `{"client_message_id":"` + uuid.NewString() + `","cuerpo":"x"}`
	if code, body := statusHTTP(ctxSup, http.MethodPost, "/api/v1/conversaciones/"+conv.Id.String()+"/mensajes", cuerpo); code != http.StatusForbidden {
		t.Fatalf("enviar como supervisor por HTTP: status %d cuerpo %s; quiere 403", code, body)
	}
	if code, body := statusHTTP(ctxSup, http.MethodPost, "/api/v1/conversaciones/"+convB.Id.String()+"/reclamos", `{"version_esperada":0}`); code != http.StatusForbidden {
		t.Fatalf("reclamar como supervisor por HTTP: status %d cuerpo %s; quiere 403", code, body)
	}

	// Nada de lo anterior necesitó filas de membresía.
	var filas int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM mensajeria.pool_membresia`).Scan(&filas); err != nil {
		t.Fatalf("contar pool_membresia: %v", err)
	}
	if filas != 0 {
		t.Fatalf("pool_membresia tiene %d filas, quiere 0", filas)
	}
}
