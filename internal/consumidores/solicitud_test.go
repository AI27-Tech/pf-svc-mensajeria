package consumidores_test

import (
	"context"
	"testing"
	"time"

	eventosv1 "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/eventos/ai27/eventos/v1"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/consumidores"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type convSolicitudFake struct {
	creadas []conversacion.EntradaCreacion
	estados []string
	cuando  []time.Time
}

func (f *convSolicitudFake) CrearSolicitudPorEvento(_ context.Context, in conversacion.EntradaCreacion) (bool, error) {
	f.creadas = append(f.creadas, in)
	return true, nil
}

func (f *convSolicitudFake) AplicarEstadoSolicitud(_ context.Context, _, _, a string, en time.Time) error {
	f.estados = append(f.estados, a)
	f.cuando = append(f.cuando, en)
	return nil
}

// sobreDe serializa el evento generado como lo publica operacion: JSON con los
// nombres del .proto y las marcas de tiempo en RFC 3339.
func sobreDe(t *testing.T, tipo string, m proto.Message) bus.Sobre {
	t.Helper()
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		t.Fatalf("serializar %s: %v", tipo, err)
	}
	return bus.Sobre{EventoID: "ev-1", Tipo: tipo, Version: 1, Payload: b}
}

func TestConsumidorSolicitudDespachaPorTipo(t *testing.T) {
	f := &convSolicitudFake{}
	c := consumidores.NuevoConsumidorSolicitud(f)
	ctx := context.Background()
	en := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	if err := c.Procesar(ctx, sobreDe(t, consumidores.TipoSolicitudCreada, &eventosv1.SolicitudCreada{
		SolicitudId: "s-1", ClienteId: "c-1", CreadaEn: timestamppb.New(en),
	})); err != nil {
		t.Fatal(err)
	}
	if len(f.creadas) != 1 || f.creadas[0].ViajeID != "s-1" || f.creadas[0].ClienteID != "c-1" {
		t.Fatalf("creadas = %+v", f.creadas)
	}

	if err := c.Procesar(ctx, sobreDe(t, consumidores.TipoSolicitudEstadoCambiado, &eventosv1.SolicitudEstadoCambiado{
		SolicitudId: "s-1", ClienteId: "c-1", DeEstado: "PAGADO", AEstado: "APROBADO", CambiadoEn: timestamppb.New(en),
	})); err != nil {
		t.Fatal(err)
	}
	if len(f.estados) != 1 || f.estados[0] != "APROBADO" || !f.cuando[0].Equal(en) {
		t.Fatalf("estados = %v %v", f.estados, f.cuando)
	}
}

// Lo ilegible o incompleto se acusa sin efecto: reintentarlo no lo arregla.
func TestConsumidorSolicitudIgnoraLoInvalido(t *testing.T) {
	f := &convSolicitudFake{}
	c := consumidores.NuevoConsumidorSolicitud(f)
	ctx := context.Background()
	for _, s := range []bus.Sobre{
		{Tipo: consumidores.TipoSolicitudCreada, Payload: []byte(`no es json`)},
		{Tipo: consumidores.TipoSolicitudCreada, Payload: []byte(`{"solicitud_id":"s-1"}`)},
		{Tipo: consumidores.TipoSolicitudEstadoCambiado, Payload: []byte(`{"solicitud_id":"s-1","cliente_id":"c-1","a_estado":"APROBADO"}`)},
		{Tipo: consumidores.TipoSolicitudEstadoCambiado, Payload: []byte(`{"solicitud_id":"s-1","cliente_id":"c-1","cambiado_en":"2026-11-20T12:00:00Z"}`)},
		{Tipo: "viaje.cerrado", Payload: []byte(`{}`)},
	} {
		if err := c.Procesar(ctx, s); err != nil {
			t.Errorf("%s: %v", s.Tipo, err)
		}
	}
	if len(f.creadas)+len(f.estados) != 0 {
		t.Fatalf("hubo efecto: %+v %v", f.creadas, f.estados)
	}
}
