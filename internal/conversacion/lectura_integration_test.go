package conversacion_test

import (
	"context"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
)

// Lectura de la conversación del viaje (E9-T8 / Ola B.0): ObtenerConversacion y
// ListarMensajes. Pruebas de COMPORTAMIENTO contra Postgres real, con valores
// literales observables. Se saltan si MENSAJERIA_TEST_DB no está definido.

func prepararLectura(t *testing.T, ctx context.Context) (*conversacion.Servicio, func()) {
	t.Helper()
	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)
	closeAdmin()

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	svc := conversacion.NewServicio(pool, outbox.NewPublisher())
	cleanup := func() {
		closePool()
		admin2, c2 := dbtest.AdminConn(t, ctx)
		dbtest.Reiniciar(t, ctx, admin2)
		c2()
	}
	return svc, cleanup
}

// TestObtenerConversacionSinMensajes — una conversación recién creada, sin
// mensajes: ultima_secuencia se clampa a 0 (no -1), version_reclamo 0, sin
// reclamo activo, atendida_en nil.
func TestObtenerConversacionSinMensajes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, cleanup := prepararLectura(t, ctx)
	defer cleanup()

	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	tenant := "c1000000-0000-0000-0000-000000000001"
	convID := "11111111-1111-1111-1111-111111111111"
	viaje := "aaaaaaaa-1111-1111-1111-111111111111"
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+convID+`','`+tenant+`','`+viaje+`','`+tenant+`','abierta', 0)`)
	closeAdmin()

	conv, err := svc.ObtenerConversacion(ctx, tenant, convID)
	if err != nil {
		t.Fatalf("obtener: %v", err)
	}
	if conv.UltimaSecuencia != 0 {
		t.Errorf("ultima_secuencia sin mensajes: got %d, want 0 (clamp)", conv.UltimaSecuencia)
	}
	if conv.VersionReclamo != 0 {
		t.Errorf("version_reclamo sin reclamo: got %d, want 0", conv.VersionReclamo)
	}
	if conv.ReclamoActivo != nil {
		t.Errorf("reclamo_activo debería ser nil sin reclamo")
	}
	if conv.AtendidaEn != nil {
		t.Errorf("atendida_en debería ser nil sin marca de atendida")
	}
	if conv.Estado != "abierta" {
		t.Errorf("estado: got %q, want abierta", conv.Estado)
	}
}

// TestObtenerConversacionConMensajesYAtendida — con mensajes y marca de atendida:
// ultima_secuencia es la última secuencia asignada; atendida_en no es nil.
func TestObtenerConversacionConMensajesYAtendida(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, cleanup := prepararLectura(t, ctx)
	defer cleanup()

	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	tenant := "c2000000-0000-0000-0000-000000000002"
	convID := "22222222-2222-2222-2222-222222222222"
	viaje := "aaaaaaaa-2222-2222-2222-222222222222"
	// siguiente_secuencia = 2 ⇒ se asignaron las secuencias 1 y 2 ⇒ ultima = 2.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+convID+`','`+tenant+`','`+viaje+`','`+tenant+`','abierta', 2)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.estado_atendido (conversacion_id, tenant_id, atendida, atendida_por, atendida_en)
		 VALUES ('`+convID+`','`+tenant+`', true, gen_random_uuid(), now())`)
	closeAdmin()

	conv, err := svc.ObtenerConversacion(ctx, tenant, convID)
	if err != nil {
		t.Fatalf("obtener: %v", err)
	}
	if conv.UltimaSecuencia != 2 {
		t.Errorf("ultima_secuencia: got %d, want 2", conv.UltimaSecuencia)
	}
	if conv.AtendidaEn == nil {
		t.Errorf("atendida_en debería estar presente tras marcar atendida")
	}
}

// TestObtenerConversacionInexistente — conversación que no existe para el tenant:
// ErrConversacionNoEncontrada.
func TestObtenerConversacionInexistente(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, cleanup := prepararLectura(t, ctx)
	defer cleanup()

	if _, err := svc.ObtenerConversacion(ctx, "cx000000-0000-0000-0000-0000000000ff", "99999999-9999-9999-9999-999999999999"); err == nil {
		t.Error("obtener una conversación inexistente debió fallar")
	}
}

// TestListarMensajesOrdenYPaginacion — tres mensajes; sin cursor devuelve los
// más recientes en orden ascendente de secuencia; con limite menor al total,
// hay_mas = true.
func TestListarMensajesOrdenYPaginacion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, cleanup := prepararLectura(t, ctx)
	defer cleanup()

	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	tenant := "c3000000-0000-0000-0000-000000000003"
	convID := "33333333-3333-3333-3333-333333333333"
	viaje := "aaaaaaaa-3333-3333-3333-333333333333"
	actor := "44444444-4444-4444-4444-444444444444"
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+convID+`','`+tenant+`','`+viaje+`','`+tenant+`','abierta', 3)`)
	for _, s := range []string{"1", "2", "3"} {
		dbtest.Exec(t, ctx, admin,
			`INSERT INTO mensajeria.mensaje (conversacion_id, tenant_id, secuencia, actor_real, client_message_id, cuerpo)
			 VALUES ('`+convID+`','`+tenant+`', `+s+`, '`+actor+`', gen_random_uuid(), 'm`+s+`')`)
	}
	closeAdmin()

	// Sin cursor, limite 2: debe traer las dos más recientes (secuencias 2 y 3)
	// en orden ascendente, y hay_mas = true.
	items, hayMas, err := svc.ListarMensajes(ctx, tenant, convID, nil, nil, 2)
	if err != nil {
		t.Fatalf("listar: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items: got %d, want 2", len(items))
	}
	if items[0].Secuencia != 2 || items[1].Secuencia != 3 {
		t.Errorf("orden ascendente esperado [2,3], got [%d,%d]", items[0].Secuencia, items[1].Secuencia)
	}
	if !hayMas {
		t.Errorf("hay_mas debería ser true con 3 mensajes y limite 2")
	}

	// despues_de = 1: trae 2 y 3 ascendente, hay_mas false (caben en el límite).
	uno := int64(1)
	items2, hayMas2, err := svc.ListarMensajes(ctx, tenant, convID, nil, &uno, 50)
	if err != nil {
		t.Fatalf("listar despues_de: %v", err)
	}
	if len(items2) != 2 || items2[0].Secuencia != 2 || items2[1].Secuencia != 3 {
		t.Errorf("despues_de=1 esperaba [2,3], got %d items", len(items2))
	}
	if hayMas2 {
		t.Errorf("hay_mas debería ser false: 2 y 3 caben en el límite")
	}

	// antes_de = 3: trae 1 y 2 ascendente.
	tres := int64(3)
	items3, _, err := svc.ListarMensajes(ctx, tenant, convID, &tres, nil, 50)
	if err != nil {
		t.Fatalf("listar antes_de: %v", err)
	}
	if len(items3) != 2 || items3[0].Secuencia != 1 || items3[1].Secuencia != 2 {
		t.Errorf("antes_de=3 esperaba [1,2] ascendente, got %d items", len(items3))
	}
}

// TestListarMensajesVacio — conversación sin mensajes: lista vacía (no nil),
// hay_mas false.
func TestListarMensajesVacio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, cleanup := prepararLectura(t, ctx)
	defer cleanup()

	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	tenant := "c4000000-0000-0000-0000-000000000004"
	convID := "55555555-5555-5555-5555-555555555555"
	viaje := "aaaaaaaa-5555-5555-5555-555555555555"
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+convID+`','`+tenant+`','`+viaje+`','`+tenant+`','abierta', 0)`)
	closeAdmin()

	items, hayMas, err := svc.ListarMensajes(ctx, tenant, convID, nil, nil, 50)
	if err != nil {
		t.Fatalf("listar: %v", err)
	}
	if items == nil {
		t.Errorf("items debería ser lista vacía, no nil")
	}
	if len(items) != 0 || hayMas {
		t.Errorf("sin mensajes: esperaba 0 items y hay_mas false, got %d / %v", len(items), hayMas)
	}
}
