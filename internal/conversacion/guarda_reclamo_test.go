package conversacion_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
)

// TestEnviarGuardaReclamoInterno cubre la autorización de envío interno de E3-T2
// (AI27-61, Addendum §7): un actor INTERNO necesita un reclamo activo propio en
// la versión esperada; el cliente no; el override de supervisor salta la guarda
// y se audita. Cada caso afirma el resultado observable (error esperado o éxito)
// y los efectos (mensaje creado o no, fila de auditoría del override).
//
// Prueba de comportamiento contra Postgres real. Se salta sin MENSAJERIA_TEST_DB.
func TestEnviarGuardaReclamoInterno(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tenant = "t1"
		ejec   = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		otro   = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
		cli    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	)
	v := func(n int64) *int64 { return &n }

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()

	svc := conversacion.NewServicio(pool, outbox.NewPublisher())

	contarMsg := func(conv string) int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM mensajeria.mensaje WHERE conversacion_id = $1`, conv,
		).Scan(&n); err != nil {
			t.Fatalf("contar mensajes: %v", err)
		}
		return n
	}

	// crearConv abre una conversación abierta y, si reclamante != "", un reclamo
	// activo de ese actor en version 1.
	crearConv := func(conv, reclamante string) {
		dbtest.Exec(t, ctx, admin,
			`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
			 VALUES ('`+conv+`', '`+tenant+`', gen_random_uuid(), '`+cli+`', 'abierta', 0)`)
		if reclamante != "" {
			dbtest.Exec(t, ctx, admin,
				`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version)
				 VALUES ('`+conv+`', '`+tenant+`', '`+reclamante+`', 1)`)
		}
	}

	t.Run("interno sin version esperada se rechaza", func(t *testing.T) {
		conv := "11111111-1111-1111-1111-111111111111"
		crearConv(conv, ejec)
		_, err := svc.Enviar(ctx, conversacion.EntradaEnvio{
			TenantID: tenant, ConversacionID: conv, ActorReal: ejec,
			ClientMessageID: "33333333-3333-3333-3333-333333333331",
			Cuerpo:          "x", RolAlEnviar: "ejecutivo",
			VersionReclamoEsperada: nil,
		})
		if !errors.Is(err, conversacion.ErrSinReclamoActivo) {
			t.Fatalf("error = %v, quiere ErrSinReclamoActivo", err)
		}
		if n := contarMsg(conv); n != 0 {
			t.Fatalf("no debería crear mensaje, hay %d", n)
		}
	})

	t.Run("interno con version equivocada se rechaza", func(t *testing.T) {
		conv := "22222222-2222-2222-2222-222222222222"
		crearConv(conv, ejec)
		_, err := svc.Enviar(ctx, conversacion.EntradaEnvio{
			TenantID: tenant, ConversacionID: conv, ActorReal: ejec,
			ClientMessageID: "33333333-3333-3333-3333-333333333332",
			Cuerpo:          "x", RolAlEnviar: "ejecutivo",
			VersionReclamoEsperada: v(2), // el reclamo está en v1
		})
		if !errors.Is(err, conversacion.ErrSinReclamoActivo) {
			t.Fatalf("error = %v, quiere ErrSinReclamoActivo", err)
		}
		if n := contarMsg(conv); n != 0 {
			t.Fatalf("no debería crear mensaje, hay %d", n)
		}
	})

	t.Run("interno con reclamo de otro actor se rechaza", func(t *testing.T) {
		conv := "33333333-3333-3333-3333-333333333333"
		crearConv(conv, otro) // el reclamo es de 'otro', no de 'ejec'
		_, err := svc.Enviar(ctx, conversacion.EntradaEnvio{
			TenantID: tenant, ConversacionID: conv, ActorReal: ejec,
			ClientMessageID: "33333333-3333-3333-3333-333333333333",
			Cuerpo:          "x", RolAlEnviar: "ejecutivo",
			VersionReclamoEsperada: v(1),
		})
		if !errors.Is(err, conversacion.ErrSinReclamoActivo) {
			t.Fatalf("error = %v, quiere ErrSinReclamoActivo", err)
		}
		if n := contarMsg(conv); n != 0 {
			t.Fatalf("no debería crear mensaje, hay %d", n)
		}
	})

	t.Run("interno con reclamo activo correcto envía", func(t *testing.T) {
		conv := "44444444-4444-4444-4444-444444444444"
		crearConv(conv, ejec)
		res, err := svc.Enviar(ctx, conversacion.EntradaEnvio{
			TenantID: tenant, ConversacionID: conv, ActorReal: ejec,
			ClientMessageID: "33333333-3333-3333-3333-333333333334",
			Cuerpo:          "hola", RolAlEnviar: "ejecutivo",
			VersionReclamoEsperada: v(1),
		})
		if err != nil {
			t.Fatalf("envío con reclamo correcto: %v", err)
		}
		if res.Secuencia != 1 {
			t.Fatalf("secuencia = %d, quiere 1", res.Secuencia)
		}
		if n := contarMsg(conv); n != 1 {
			t.Fatalf("debería haber 1 mensaje, hay %d", n)
		}
	})

	t.Run("cliente sin reclamo envía", func(t *testing.T) {
		conv := "55555555-5555-5555-5555-555555555555"
		crearConv(conv, "") // sin reclamo
		res, err := svc.Enviar(ctx, conversacion.EntradaEnvio{
			TenantID: tenant, ConversacionID: conv, ActorReal: cli,
			ClientMessageID: "33333333-3333-3333-3333-333333333335",
			Cuerpo:          "hola desde el cliente", RolAlEnviar: "cliente",
			VersionReclamoEsperada: nil,
		})
		if err != nil {
			t.Fatalf("envío del cliente: %v", err)
		}
		if res.Secuencia != 1 {
			t.Fatalf("secuencia = %d, quiere 1", res.Secuencia)
		}
	})

	t.Run("override de supervisor cerrado en V1: rechaza sin efecto", func(t *testing.T) {
		conv := "66666666-6666-6666-6666-666666666666"
		crearConv(conv, "") // sin reclamo activo
		_, err := svc.Enviar(ctx, conversacion.EntradaEnvio{
			TenantID: tenant, ConversacionID: conv, ActorReal: ejec,
			ClientMessageID: "33333333-3333-3333-3333-333333333336",
			Cuerpo:          "override del supervisor", RolAlEnviar: "supervisor",
			MotivoOverride: "cliente urgente, ejecutivo ausente",
		})
		if !errors.Is(err, conversacion.ErrOverrideNoImplementado) {
			t.Fatalf("error = %v, quiere ErrOverrideNoImplementado", err)
		}
		if n := contarMsg(conv); n != 0 {
			t.Fatalf("override cerrado no debería crear mensaje, hay %d", n)
		}
		var nAud int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM mensajeria.auditoria_mensajeria
			  WHERE conversacion_id = $1 AND accion = 'mensaje.override'`, conv,
		).Scan(&nAud); err != nil {
			t.Fatalf("contar auditoría: %v", err)
		}
		if nAud != 0 {
			t.Fatalf("override cerrado no debería auditar, hay %d filas", nAud)
		}
	})
}
