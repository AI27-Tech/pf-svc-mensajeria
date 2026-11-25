package mensajeria_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	platformpgx "github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	"github.com/AI27-Tech/pf-svc-mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/google/uuid"
)

func TestAdministradorChatViaje(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dbtest.Serializar(t, ctx)
	db, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, db)
	defer dbtest.Reiniciar(t, ctx, db)
	dbtest.AplicarMigraciones(t, ctx, db)
	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()

	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	api := mod.Conversaciones()
	mux := http.NewServeMux()
	mod.Routes(mux)
	como := func(sub uuid.UUID, rol string) context.Context {
		return auth.ContextWithClaims(ctx, &auth.Claims{Subject: sub.String(), TenantID: "ai27", Rol: rol, Audiencia: auth.AudienciaInterno})
	}
	adm := uuid.New()
	ctxAdm, ctxEj := como(adm, "administrador"), como(uuid.New(), "ejecutivo")
	pedir := func(t *testing.T, method, path string, body any, status int) []byte {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewReader(payload)).WithContext(ctxAdm))
		if rec.Code != status {
			t.Errorf("%s %s: HTTP %d, quiere %d; cuerpo %s", method, path, rec.Code, status, rec.Body)
		}
		return rec.Body.Bytes()
	}
	categoria := func(t *testing.T, err error, want platformerrors.Category) {
		t.Helper()
		if platformerrors.AsCategory(err) != want {
			t.Errorf("error %v; quiere categoría %v", err, want)
		}
	}

	fixture := conversacion.NewServicio(pool, outbox.NewPublisher())
	empresa, viaje := uuid.NewString(), uuid.New()
	nuevoViaje := func(empresa string, id uuid.UUID) uuid.UUID {
		t.Helper()
		if _, err := fixture.CrearPorEvento(ctx, conversacion.EntradaCreacion{ClienteID: empresa, ViajeID: id.String()}); err != nil {
			t.Fatal(err)
		}
		c, err := api.ObtenerConversacionDeViaje(ctxEj, id)
		if err != nil {
			t.Fatal(err)
		}
		return c.Id
	}
	id := nuevoViaje(empresa, viaje)
	_, err = fixture.CrearSolicitudPorEvento(ctx, conversacion.EntradaCreacion{ClienteID: empresa, ViajeID: viaje.String()})
	if err != nil {
		t.Fatal(err)
	}
	solicitud, err := api.ObtenerConversacionDeSolicitud(ctxAdm, viaje)
	if err != nil {
		t.Fatal(err)
	}
	ruta := "/api/v1/conversaciones/" + id.String()
	bandeja := "/api/v1/bandeja/trabajo"
	verBandeja := func(t *testing.T, data []byte, want ...uuid.UUID) {
		t.Helper()
		var b msgclient.TrabajoBandeja
		if err := json.Unmarshal(data, &b); err != nil || len(b.Items) != len(want) {
			t.Errorf("bandeja %s: quiere solo %v (error %v)", data, want, err)
			return
		}
		for i, item := range b.Items {
			if item.Id != want[i] {
				t.Errorf("bandeja: item %d = %s; quiere %s", i, item.Id, want[i])
			}
		}
	}
	t.Run("lee solo viajes en la bandeja y abre el chat", func(t *testing.T) {
		verBandeja(t, pedir(t, http.MethodGet, bandeja, nil, http.StatusOK), id)
		b, err := api.ListarTrabajoBandeja(ctxAdm, msgclient.ListarTrabajoMiBandejaParams{})
		if err != nil || len(b.Items) != 1 || b.Items[0].Id != id {
			t.Errorf("bandeja en proceso: %+v, %v; quiere solo el viaje", b, err)
		}
		c, err := api.ObtenerConversacionDeViaje(ctxAdm, viaje)
		if err != nil || c.Id != id {
			t.Errorf("lectura por viaje: %+v, %v; quiere %s", c, err, id)
		} else if c.BandejaId != nil {
			pedir(t, http.MethodGet, "/api/v1/bandejas/"+c.BandejaId.String()+"/trabajo", nil, http.StatusForbidden)
		}
		pedir(t, http.MethodGet, ruta, nil, http.StatusOK)
		pedir(t, http.MethodPost, "/api/v1/conversaciones/"+solicitud.Id.String()+"/reclamos",
			msgclient.VersionEsperadaEntrada{}, http.StatusForbidden)
	})

	envio := func(v int64) msgclient.EnviarMensajeEntrada {
		return msgclient.EnviarMensajeEntrada{Cuerpo: "Seguimos en ruta", ClientMessageId: uuid.New(), VersionReclamoEsperada: &v}
	}
	t.Run("reclama, escribe y libera su propio reclamo", func(t *testing.T) {
		_, err := api.EnviarMensaje(ctxAdm, id, envio(0))
		categoria(t, err, platformerrors.CategoryConflicto)
		data := pedir(t, http.MethodPost, ruta+"/reclamos", msgclient.VersionEsperadaEntrada{}, http.StatusCreated)
		var r msgclient.Reclamo
		if err := json.Unmarshal(data, &r); err != nil || r.ResponsableId != adm || r.Version != 1 {
			t.Fatalf("reclamo %s: quiere responsable %s y versión 1 (error %v)", data, adm, err)
		}
		m, err := api.EnviarMensaje(ctxAdm, id, envio(1))
		if err != nil || m.Cuerpo != "Seguimos en ruta" || m.RolAlEnviar == nil || *m.RolAlEnviar != "administrador" {
			t.Errorf("mensaje en proceso: %+v, %v; quiere el texto y el rol real", m, err)
		}
		pag, err := api.ListarMensajes(ctxAdm, id, msgclient.ListarMensajesParams{})
		if err != nil || len(pag.Items) != 1 || pag.Items[0].Cuerpo != "Seguimos en ruta" {
			t.Errorf("historial: %+v, %v; quiere solo el mensaje enviado", pag, err)
		}
		pedir(t, http.MethodPost, ruta+"/transferencias", msgclient.TransferenciaEntrada{ResponsableDestinoId: uuid.New(), VersionEsperada: 1}, http.StatusForbidden)
		pedir(t, http.MethodPost, ruta+"/atendida", msgclient.VersionEsperadaEntrada{VersionEsperada: 1}, http.StatusForbidden)
		pedir(t, http.MethodPost, ruta+"/recibos/entregado", msgclient.ReciboEntrada{HastaSecuencia: 1}, http.StatusForbidden)
		pedir(t, http.MethodPost, ruta+"/recibos/leido", msgclient.ReciboEntrada{HastaSecuencia: 1}, http.StatusForbidden)
		err = api.LiberarReclamo(ctxAdm, id, r.Id, msgclient.LiberarReclamoParams{VersionEsperada: 1})
		if err != nil {
			t.Fatalf("liberar reclamo propio en proceso: %v", err)
		}
		c, err := api.ObtenerConversacion(ctxEj, id)
		if err != nil || c.ReclamoActivo != nil || c.VersionReclamo != 1 {
			t.Errorf("tras liberar: %+v, %v; quiere versión 1 sin reclamo activo", c, err)
		}

		r2, err := api.ReclamarConversacion(ctxAdm, id, msgclient.VersionEsperadaEntrada{VersionEsperada: 1})
		if err != nil || r2.ResponsableId != adm || r2.Version != 2 {
			t.Fatalf("reclamo en proceso: %+v, %v; quiere el mismo responsable en versión 2", r2, err)
		}
		pedir(t, http.MethodPost, ruta+"/mensajes", envio(2), http.StatusCreated)
		pedir(t, http.MethodDelete, ruta+"/reclamos/"+r2.Id.String()+"?version_esperada=2", nil, http.StatusNoContent)
	})

	t.Run("lee el chat visible sin usar el reclamo de terceros", func(t *testing.T) {
		otraEmpresa := uuid.NewString()
		ajeno := nuevoViaje(otraEmpresa, uuid.New())
		rutaAjena := "/api/v1/conversaciones/" + ajeno.String()
		r, err := api.ReclamarConversacion(ctxEj, ajeno, msgclient.VersionEsperadaEntrada{})
		if err != nil {
			t.Fatal(err)
		}
		pedir(t, http.MethodGet, rutaAjena, nil, http.StatusOK)
		pag, err := api.ListarMensajes(ctxAdm, ajeno, msgclient.ListarMensajesParams{})
		if err != nil || len(pag.Items) != 0 {
			t.Fatalf("historial sin mensajes: %+v, %v; quiere una página vacía", pag, err)
		}
		pedir(t, http.MethodPost, rutaAjena+"/mensajes", envio(1), http.StatusConflict)
		pedir(t, http.MethodDelete, rutaAjena+"/reclamos/"+r.Id.String()+"?version_esperada=1", nil, http.StatusForbidden)
		c, err := api.ObtenerConversacion(ctxEj, ajeno)
		if err != nil || c.ReclamoActivo == nil || c.ReclamoActivo.Id != r.Id {
			t.Errorf("reclamo después de los rechazos: %+v, %v; quiere conservar %s", c, err, r.Id)
		}

		// El siguiente viaje de la empresa nace dirigido al Ejecutivo disponible.
		dirigido := nuevoViaje(otraEmpresa, uuid.New())
		verBandeja(t, pedir(t, http.MethodGet, bandeja, nil, http.StatusOK), id, ajeno)
		verBandeja(t, pedir(t, http.MethodGet, bandeja+"?desde_revision=3", nil, http.StatusOK))
		_, err = api.ObtenerConversacion(ctxAdm, dirigido)
		categoria(t, err, platformerrors.CategoryProhibido)
		_, err = api.ReclamarConversacion(ctxAdm, dirigido, msgclient.VersionEsperadaEntrada{})
		categoria(t, err, platformerrors.CategoryConflicto)
	})
}
