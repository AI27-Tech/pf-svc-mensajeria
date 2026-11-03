package mensajeria_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// DEC-127) sin sembrar bandejas, rutas ni membresías: viaje.mensajeria_habilitada
// crea la conversación por el consumidor del módulo; la conversación aparece en
// la bandeja del Ejecutivo (en proceso y HTTP); el Ejecutivo la reclama; una
// persona de la empresa cliente (sub ≠ cliente_id) la abre por viaje y escribe;
// un monitorista recibe las mismas denegaciones que antes sin membresía.
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
		viaje1      = "71000000-0000-0000-0000-000000000001"
		viaje2      = "72000000-0000-0000-0000-000000000002"
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

	habilitar := func(viaje string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"viaje_id": viaje, "cliente_id": empresa})
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
		return auth.ContextWithClaims(ctx, &auth.Claims{Subject: s, TenantID: empresa, Rol: rol, Audiencia: aud})
	}
	ctxEjec := como(ejecutivo, "ejecutivo", auth.AudienciaInterno)
	ctxMon := como(monitorista, "monitorista", auth.AudienciaInterno)
	ctxPersona := como(persona, "cliente", auth.AudienciaCliente)
	statusHTTP := func(ctxReq context.Context, metodo, ruta string) (int, []byte) {
		req := httptest.NewRequest(metodo, ruta, nil).WithContext(ctxReq)
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

	// 5. Un monitorista: misma denegación que antes sin membresía.
	if _, err := api.ListarTrabajoBandeja(ctxMon, msgclient.ListarTrabajoMiBandejaParams{}); categoria(err) != platformerrors.CategoryNoEncontrado {
		t.Fatalf("bandeja de monitorista: categoría %v, quiere no_encontrado", categoria(err))
	}
	if _, err := api.ObtenerConversacion(ctxMon, conv.Id); categoria(err) != platformerrors.CategoryNoAutorizado {
		t.Fatalf("conversación como monitorista: categoría %v, quiere no_autorizado", categoria(err))
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
