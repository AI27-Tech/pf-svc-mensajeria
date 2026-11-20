package mensajeria_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	eventosv1 "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/eventos/ai27/eventos/v1"
	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	platformpgx "github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/pool"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestPoolCotizacion ejerce, por HTTP y contra Postgres real, el pool de
// `cotizacion` sobre la conversación de la solicitud (E10-HU-038): un solo pool
// sin vínculo por cliente, reclamo concurrente con un ganador, respuesta solo del
// reclamante, transferencia atómica solo a `cotizacion`, intervención auditada del
// administrador, audiencia por tipo en cada operación por {id}, el aviso de mensaje
// sin respuesta y que la conversación del viaje y la de la solicitud no se mezclen.
func TestPoolCotizacion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		cliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		id      = "11111111-1111-1111-1111-111111111111" // solicitud y viaje: mismo id
	)
	cot1, cot2, cot3 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	adm, eje := uuid.NewString(), uuid.NewString()

	rp, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := conversacion.NewServicio(rp, outbox.NewPublisher())
	in := conversacion.EntradaCreacion{ViajeID: id, ClienteID: cliente}
	if _, err := svc.CrearSolicitudPorEvento(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CrearPorEvento(ctx, in); err != nil {
		t.Fatal(err)
	}
	convSol, _ := svc.ConversacionDeSolicitud(ctx, id)
	convViaje, _ := svc.ConversacionDeViaje(ctx, id)

	// Los roles llegan por los eventos de identidad, construidos con los tipos
	// generados del contrato, como los publica `identidad`.
	roles := pool.NuevoConsumidorRoles(rp)
	publicar := func(tipo string, js []byte) {
		t.Helper()
		if err := roles.Procesar(ctx, bus.Sobre{EventoID: uuid.NewString(), Tipo: tipo, Version: 1, Payload: js}); err != nil {
			t.Fatalf("%s: %v", tipo, err)
		}
	}
	hecho := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	asignar := func(sujeto, rol string, en time.Time) {
		t.Helper()
		js, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&eventosv1.RolAsignado{SujetoId: sujeto, TenantId: "ai27", Rol: rol, AsignadoEn: timestamppb.New(en)})
		publicar(pool.TipoRolAsignado, js)
	}
	for _, c := range [][2]string{{cot1, "cotizacion"}, {cot2, "cotizacion"}, {eje, "ejecutivo"}, {adm, "administrador"}} {
		asignar(c[0], c[1], hecho)
	}

	chat := &platformpgx.Pool{Pool: rp}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{})
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	mux := http.NewServeMux()
	mod.Routes(mux)

	type resp struct {
		code   int
		cuerpo []byte
	}
	llamar := func(sub, rol string, aud auth.Audiencia, metodo, ruta string, cuerpo any) resp {
		t.Helper()
		var b []byte
		if cuerpo != nil {
			b, _ = json.Marshal(cuerpo)
		}
		tenant := "ai27"
		if aud == auth.AudienciaCliente {
			tenant = cliente
		}
		req := httptest.NewRequest(metodo, ruta, bytes.NewReader(b)).WithContext(
			auth.ContextWithClaims(ctx, &auth.Claims{Subject: sub, TenantID: tenant, Rol: rol, Audiencia: aud}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return resp{rec.Code, rec.Body.Bytes()}
	}
	como := func(sub, rol string) func(string, string, any) resp {
		return func(metodo, ruta string, cuerpo any) resp {
			return llamar(sub, rol, auth.AudienciaInterno, metodo, ruta, cuerpo)
		}
	}
	elCliente := func(metodo, ruta string, cuerpo any) resp {
		return llamar(cliente, "cliente", auth.AudienciaCliente, metodo, ruta, cuerpo)
	}
	codigo := func(r resp) string {
		var e struct {
			Codigo string `json:"codigo"`
		}
		_ = json.Unmarshal(r.cuerpo, &e)
		return e.Codigo
	}
	conv := func(r resp) msgclient.Conversacion {
		t.Helper()
		var c msgclient.Conversacion
		if r.code != http.StatusOK {
			t.Fatalf("obtener: %d %s", r.code, r.cuerpo)
		}
		_ = json.Unmarshal(r.cuerpo, &c)
		return c
	}
	reclamoDe := func(r resp) msgclient.Reclamo {
		t.Helper()
		var c msgclient.Reclamo
		if r.code != http.StatusCreated {
			t.Fatalf("reclamo: %d %s", r.code, r.cuerpo)
		}
		_ = json.Unmarshal(r.cuerpo, &c)
		return c
	}
	rutaSol := "/api/v1/conversaciones/" + convSol
	rutaViaje := "/api/v1/conversaciones/" + convViaje
	enviar := func(ver *int64, texto string) map[string]any {
		return map[string]any{"client_message_id": uuid.NewString(), "cuerpo": texto, "version_reclamo_esperada": ver}
	}
	version := func(v int64) map[string]any { return map[string]any{"version_esperada": v} }

	// --- Audiencia por tipo en CADA operación por {id} -------------------------
	t.Run("los roles sin acceso reciben 403 en cada operación de la solicitud", func(t *testing.T) {
		rid := uuid.NewString()
		operaciones := []struct {
			nombre, metodo, ruta string
			cuerpo               any
		}{
			{"obtenerConversacion", "GET", rutaSol, nil},
			{"listarMensajes", "GET", rutaSol + "/mensajes", nil},
			{"enviarMensaje", "POST", rutaSol + "/mensajes", enviar(nil, "x")},
			{"reclamarConversacion", "POST", rutaSol + "/reclamos", version(0)},
			{"liberarReclamo", "DELETE", rutaSol + "/reclamos/" + rid + "?version_esperada=1", nil},
			{"transferirConversacion", "POST", rutaSol + "/transferencias", map[string]any{"version_esperada": 1, "responsable_destino_id": cot1}},
			{"marcarAtendida", "POST", rutaSol + "/atendida", version(1)},
			{"registrarReciboEntregado", "POST", rutaSol + "/recibos/entregado", map[string]any{"hasta_secuencia": 1}},
			{"registrarReciboLeido", "POST", rutaSol + "/recibos/leido", map[string]any{"hasta_secuencia": 1}},
		}
		for _, rol := range []string{"ejecutivo", "supervisor", "coordinador", "jefe_operativo", "monitorista", "reaccion"} {
			for _, op := range operaciones {
				if r := como(uuid.NewString(), rol)(op.metodo, op.ruta, op.cuerpo); r.code != http.StatusForbidden {
					t.Errorf("%s en %s: %d, quiere 403", rol, op.nombre, r.code)
				}
			}
		}
		// El tipo viaje no cambia: el ejecutivo lo lee y `cotizacion` no.
		if r := como(eje, "ejecutivo")("GET", rutaViaje, nil); r.code != http.StatusOK {
			t.Errorf("ejecutivo lee la conversación del viaje: %d", r.code)
		}
		if r := como(cot1, "cotizacion")("GET", rutaViaje, nil); r.code != http.StatusForbidden {
			t.Errorf("cotizacion en la conversación del viaje: %d, quiere 403", r.code)
		}
	})

	// --- Dos reclamos concurrentes: un ganador y un conflicto ------------------
	var ganador, perdedor func(string, string, any) resp
	var rec msgclient.Reclamo
	t.Run("dos reclamos concurrentes producen un ganador y un conflicto de versión", func(t *testing.T) {
		c1, c2 := como(cot1, "cotizacion"), como(cot2, "cotizacion")
		var wg sync.WaitGroup
		var r1, r2 resp
		inicio := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-inicio; r1 = c1("POST", rutaSol+"/reclamos", version(0)) }()
		go func() { defer wg.Done(); <-inicio; r2 = c2("POST", rutaSol+"/reclamos", version(0)) }()
		close(inicio)
		wg.Wait()
		switch {
		case r1.code == http.StatusCreated && r2.code == http.StatusConflict:
			ganador, perdedor, rec = c1, c2, reclamoDe(r1)
			if codigo(r2) != "conflicto_version_reclamo" {
				t.Errorf("codigo del perdedor = %q", codigo(r2))
			}
		case r2.code == http.StatusCreated && r1.code == http.StatusConflict:
			ganador, perdedor, rec = c2, c1, reclamoDe(r2)
			if codigo(r1) != "conflicto_version_reclamo" {
				t.Errorf("codigo del perdedor = %q", codigo(r1))
			}
		default:
			t.Fatalf("reclamos concurrentes: %d y %d; quiere un 201 y un 409", r1.code, r2.code)
		}
		// Sin vínculo por cliente: el reclamo de `cotizacion` no crea dueño del cliente.
		var duenos int
		if err := rp.QueryRow(ctx, `SELECT count(*) FROM mensajeria.dueno_cliente`).Scan(&duenos); err != nil || duenos != 0 {
			t.Errorf("dueno_cliente: %d filas (err %v); quiere 0", duenos, err)
		}
	})

	// --- Solo el reclamante responde ------------------------------------------
	t.Run("solo el reclamante activo responde", func(t *testing.T) {
		if r := perdedor("POST", rutaSol+"/mensajes", enviar(&rec.Version, "sin reclamo")); r.code != http.StatusConflict || codigo(r) != "conflicto_version_reclamo" {
			t.Errorf("responder sin reclamo: %d %s; quiere 409 conflicto_version_reclamo", r.code, codigo(r))
		}
		if r := elCliente("POST", rutaSol+"/mensajes", enviar(nil, "¿cuánto cuesta?")); r.code != http.StatusCreated {
			t.Fatalf("el cliente escribe: %d %s", r.code, r.cuerpo)
		}
		if r := ganador("POST", rutaSol+"/mensajes", enviar(&rec.Version, "$1000")); r.code != http.StatusCreated {
			t.Fatalf("el reclamante responde: %d %s", r.code, r.cuerpo)
		}
	})

	// --- sin_respuesta ---------------------------------------------------------
	t.Run("el aviso de mensaje sin respuesta se activa y se detiene", func(t *testing.T) {
		sinRespuesta := func() (interno bool, cli *bool) {
			c := conv(como(cot1, "cotizacion")("GET", rutaSol, nil))
			if c.SinRespuesta == nil {
				t.Fatal("interno: sin_respuesta ausente")
			}
			return *c.SinRespuesta, conv(elCliente("GET", rutaSol, nil)).SinRespuesta
		}
		if got, _ := sinRespuesta(); got {
			t.Fatal("tras la respuesta de AI27 no debe haber aviso")
		}
		if r := elCliente("POST", rutaSol+"/mensajes", enviar(nil, "¿y la fecha?")); r.code != http.StatusCreated {
			t.Fatal(r.code)
		}
		interno, cli := sinRespuesta()
		if !interno {
			t.Error("tras el mensaje del cliente el aviso debe estar activo de inmediato")
		}
		if cli != nil {
			t.Error("el Cliente no ve sin_respuesta")
		}
		// También en la bandeja de solicitudes del pool.
		var tb msgclient.TrabajoBandeja
		r := como(cot2, "cotizacion")("GET", "/api/v1/bandeja/solicitudes", nil)
		_ = json.Unmarshal(r.cuerpo, &tb)
		if len(tb.Items) != 1 || tb.Items[0].SinRespuesta == nil || !*tb.Items[0].SinRespuesta {
			t.Errorf("bandeja de solicitudes: %s", r.cuerpo)
		}
		if r := ganador("POST", rutaSol+"/mensajes", enviar(&rec.Version, "el 25")); r.code != http.StatusCreated {
			t.Fatal(r.code)
		}
		if got, _ := sinRespuesta(); got {
			t.Error("la respuesta de AI27 debe detener el aviso")
		}
	})

	// --- La solicitud y el viaje no se mezclan ---------------------------------
	t.Run("la conversación del viaje no recibe los mensajes de la solicitud", func(t *testing.T) {
		var p msgclient.PaginaMensajes
		r := como(eje, "ejecutivo")("GET", rutaViaje+"/mensajes", nil)
		_ = json.Unmarshal(r.cuerpo, &p)
		if r.code != http.StatusOK || len(p.Items) != 0 {
			t.Errorf("mensajes del viaje: %d, %d items; quiere 0", r.code, len(p.Items))
		}
	})

	// --- Un usuario nuevo del rol lee el historial previo ---------------------
	t.Run("un usuario nuevo de cotizacion lee el historial previo", func(t *testing.T) {
		asignar(cot3, "cotizacion", hecho)
		var p msgclient.PaginaMensajes
		r := como(cot3, "cotizacion")("GET", rutaSol+"/mensajes", nil)
		_ = json.Unmarshal(r.cuerpo, &p)
		if r.code != http.StatusOK || len(p.Items) != 4 {
			t.Errorf("historial: %d, %d mensajes; quiere 4", r.code, len(p.Items))
		}
	})

	// --- Transferencia ---------------------------------------------------------
	var nuevo msgclient.Reclamo
	t.Run("transferencia atómica a otro usuario de cotizacion", func(t *testing.T) {
		destino := cot2
		if rec.ResponsableId.String() == cot2 {
			destino = cot1
		}
		r := ganador("POST", rutaSol+"/transferencias", map[string]any{"version_esperada": rec.Version, "responsable_destino_id": destino, "motivo": "turno"})
		nuevo = reclamoDe(r)
		if nuevo.ResponsableId.String() != destino || nuevo.Version != rec.Version+1 {
			t.Fatalf("reclamo nuevo = %+v", nuevo)
		}
		var activos, cerrados int
		if err := rp.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE cerrado_en IS NULL), count(*) FILTER (WHERE cerrado_en IS NOT NULL)
			   FROM mensajeria.reclamo WHERE conversacion_id = $1`, convSol).Scan(&activos, &cerrados); err != nil || activos != 1 || cerrados != 1 {
			t.Errorf("reclamos activos/cerrados = %d/%d (err %v); quiere 1/1", activos, cerrados, err)
		}
		// El que transfirió ya no responde; el destino sí.
		if r := ganador("POST", rutaSol+"/mensajes", enviar(&rec.Version, "tarde")); r.code != http.StatusConflict {
			t.Errorf("responder tras transferir: %d, quiere 409", r.code)
		}
	})

	t.Run("transferir a un ejecutivo es 409 destino_no_es_cotizacion y el reclamo no cambia", func(t *testing.T) {
		dueno := como(nuevo.ResponsableId.String(), "cotizacion")
		for _, destino := range []string{eje, adm, uuid.NewString()} { // ejecutivo, administrador, desconocido
			r := dueno("POST", rutaSol+"/transferencias", map[string]any{"version_esperada": nuevo.Version, "responsable_destino_id": destino})
			if r.code != http.StatusConflict || codigo(r) != "destino_no_es_cotizacion" {
				t.Errorf("destino %s: %d %s; quiere 409 destino_no_es_cotizacion", destino, r.code, r.cuerpo)
			}
		}
		var actor string
		var ver int64
		if err := rp.QueryRow(ctx, `SELECT actor_real::text, version FROM mensajeria.reclamo WHERE conversacion_id = $1 AND cerrado_en IS NULL`, convSol).Scan(&actor, &ver); err != nil ||
			actor != nuevo.ResponsableId.String() || ver != nuevo.Version {
			t.Errorf("el reclamo cambió: %s v%d (err %v)", actor, ver, err)
		}
	})

	t.Run("un destino al que se le revoca el rol o se desactiva deja de ser válido", func(t *testing.T) {
		dueno := como(nuevo.ResponsableId.String(), "cotizacion")
		otro := cot1
		if nuevo.ResponsableId.String() == cot1 {
			otro = cot2
		}
		transferible := func() int {
			return dueno("POST", rutaSol+"/transferencias", map[string]any{"version_esperada": nuevo.Version, "responsable_destino_id": otro}).code
		}
		desactivar := func(en time.Time) {
			js, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&eventosv1.SujetoDesactivado{SujetoId: otro, TenantId: "ai27", DesactivadoEn: timestamppb.New(en)})
			publicar(pool.TipoSujetoDesactivado, js)
		}
		desactivar(hecho.Add(time.Hour))
		if got := transferible(); got != http.StatusConflict {
			t.Errorf("destino desactivado: %d, quiere 409", got)
		}
		js, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&eventosv1.SujetoReactivado{SujetoId: otro, TenantId: "ai27", ReactivadoEn: timestamppb.New(hecho.Add(2 * time.Hour))})
		publicar(pool.TipoSujetoReactivado, js)
		desactivar(hecho.Add(time.Hour)) // reentrega vieja: no pisa a la reactivación
		js, _ = protojson.MarshalOptions{UseProtoNames: true}.Marshal(&eventosv1.RolRevocado{SujetoId: otro, TenantId: "ai27", Rol: "cotizacion", RevocadoEn: timestamppb.New(hecho.Add(3 * time.Hour))})
		publicar(pool.TipoRolRevocado, js)
		if got := transferible(); got != http.StatusConflict {
			t.Errorf("destino sin rol: %d, quiere 409", got)
		}
		asignar(otro, "cotizacion", hecho.Add(4*time.Hour))
		if got := transferible(); got != http.StatusCreated {
			t.Errorf("destino con el rol de vuelta: %d, quiere 201", got)
		}
		nuevo = reclamoActivo(t, ctx, rp, convSol)
	})

	// --- Intervención del administrador ---------------------------------------
	t.Run("el administrador envía y transfiere, y queda auditado como override", func(t *testing.T) {
		a := como(adm, "administrador")
		if r := a("POST", rutaSol+"/mensajes", enviar(nil, "interviene")); r.code != http.StatusCreated {
			t.Fatalf("el administrador envía: %d %s", r.code, r.cuerpo)
		}
		destino := cot3
		if nuevo.ResponsableId.String() == cot3 {
			destino = cot1
		}
		r := a("POST", rutaSol+"/transferencias", map[string]any{"version_esperada": nuevo.Version, "responsable_destino_id": destino})
		if got := reclamoDe(r); got.ResponsableId.String() != destino {
			t.Errorf("transferido a %s, quiere %s", got.ResponsableId, destino)
		}
		var n int
		if err := rp.QueryRow(ctx,
			`SELECT count(*) FROM mensajeria.auditoria_mensajeria
			  WHERE conversacion_id = $1 AND accion = 'override.administrador' AND actor_real = $2`, convSol, adm).Scan(&n); err != nil || n != 2 {
			t.Errorf("filas de override del administrador: %d (err %v); quiere 2 (envío y transferencia)", n, err)
		}
		// El administrador no transfiere con una versión vieja ni a un ejecutivo.
		if r := a("POST", rutaSol+"/transferencias", map[string]any{"version_esperada": nuevo.Version, "responsable_destino_id": cot1}); r.code != http.StatusConflict {
			t.Errorf("versión vieja: %d, quiere 409", r.code)
		}
	})

	// --- Ventana de escritura por HTTP ---------------------------------------
	t.Run("pasada la ventana la conversación es de solo lectura y el historial sigue visible", func(t *testing.T) {
		if err := svc.AplicarEstadoSolicitud(ctx, id, cliente, "APROBADO", time.Now().Add(-73*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if r := elCliente("POST", rutaSol+"/mensajes", enviar(nil, "tarde")); r.code != http.StatusConflict || codigo(r) != "conversacion_solo_lectura" {
			t.Errorf("cliente tras la ventana: %d %s", r.code, codigo(r))
		}
		a := como(adm, "administrador")
		if r := a("POST", rutaSol+"/mensajes", enviar(nil, "tarde")); r.code != http.StatusConflict || codigo(r) != "conversacion_solo_lectura" {
			t.Errorf("administrador tras la ventana: %d %s", r.code, codigo(r))
		}
		if r := a("GET", rutaSol+"/mensajes", nil); r.code != http.StatusOK {
			t.Errorf("historial tras la ventana: %d", r.code)
		}
	})
}

// reclamoActivo lee el reclamo activo de la conversación directamente de la tabla.
func reclamoActivo(t *testing.T, ctx context.Context, rp *pgxpool.Pool, conv string) msgclient.Reclamo {
	t.Helper()
	var r msgclient.Reclamo
	if err := rp.QueryRow(ctx,
		`SELECT id, conversacion_id, actor_real, version, abierto_en
		   FROM mensajeria.reclamo WHERE conversacion_id = $1 AND cerrado_en IS NULL`, conv,
	).Scan(&r.Id, &r.ConversacionId, &r.ResponsableId, &r.Version, &r.AbiertoEn); err != nil {
		t.Fatalf("reclamo activo: %v", err)
	}
	return r
}
