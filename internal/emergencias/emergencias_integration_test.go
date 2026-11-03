package emergencias_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/emergencias"
)

const (
	tenant    = "t1"
	viaje     = "11111111-1111-1111-1111-111111111111"
	cliente   = "22222222-2222-2222-2222-222222222222"
	bandeja   = "33333333-3333-3333-3333-333333333333"
	actorA    = "44444444-4444-4444-4444-444444444444"
	actorB    = "55555555-5555-5555-5555-555555555555"
	activacn1 = "66666666-6666-6666-6666-666666666666"
	activacn2 = "77777777-7777-7777-7777-777777777777"
)

func setup(t *testing.T, ctx context.Context) (*emergencias.Servicio, func()) {
	t.Helper()
	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)
	pool, closePool := dbtest.PoolComoRol(t, ctx)
	// Sembrar la bandeja de Reacción del tenant (tipo 'emergencias'): Activar la
	// resuelve internamente, no la recibe del llamante.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre)
		 VALUES ('`+bandeja+`', '`+tenant+`', 'emergencias', 'Reacción')`)
	svc := emergencias.NewServicio(pool, outbox.NewPublisher())
	return svc, func() {
		closePool()
		dbtest.Reiniciar(t, ctx, admin)
		closeAdmin()
	}
}

func activar(t *testing.T, ctx context.Context, svc *emergencias.Servicio, activacionID string) emergencias.ResultadoActivacion {
	t.Helper()
	res, err := svc.Activar(ctx, emergencias.EntradaActivacion{
		TenantID: tenant, ViajeID: viaje, ClienteID: cliente,
		ActorReal: actorA, Origen: msgclient.OrigenActivacionOperacion, ActivacionID: activacionID,
	})
	if err != nil {
		t.Fatalf("activar: %v", err)
	}
	return res
}

// TestActivarEsIdempotentePorActivacion afirma: reentregar la misma activacion_id
// devuelve el MISMO caso (Idempotente=true) y no crea un segundo; una activación
// distinta crea otro caso (ADR 21 §2).
func TestActivarEsIdempotentePorActivacion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, teardown := setup(t, ctx)
	defer teardown()

	primera := activar(t, ctx, svc, activacn1)
	if primera.Idempotente {
		t.Fatal("la primera activación no debe ser idempotente")
	}

	reentrega := activar(t, ctx, svc, activacn1)
	if !reentrega.Idempotente {
		t.Fatal("reentregar la misma activación debe ser idempotente")
	}
	if reentrega.EmergenciaID != primera.EmergenciaID {
		t.Fatalf("la reentrega debe devolver el mismo caso: %s != %s", reentrega.EmergenciaID, primera.EmergenciaID)
	}

	otra := activar(t, ctx, svc, activacn2)
	if otra.EmergenciaID == primera.EmergenciaID {
		t.Fatal("una activación nueva debe crear otro caso")
	}
}

// TestReclamoConcurrenteProduceGanadorYConflicto afirma: dos reclamos
// concurrentes sobre el caso recién activado (versión 0) producen exactamente un
// ganador (versión 1) y un conflicto.
func TestReclamoConcurrenteProduceGanadorYConflicto(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, teardown := setup(t, ctx)
	defer teardown()

	caso := activar(t, ctx, svc, activacn1)

	type salida struct {
		version int64
		err     error
	}
	var wg sync.WaitGroup
	res := make([]salida, 2)
	actores := []string{actorA, actorB}
	inicio := make(chan struct{})
	for i := range actores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-inicio
			v, err := svc.Reclamar(ctx, tenant, caso.EmergenciaID, actores[i], 0)
			res[i] = salida{version: int64(v), err: err}
		}(i)
	}
	close(inicio)
	wg.Wait()

	var ganadores, conflictos int
	for _, r := range res {
		switch {
		case r.err == nil:
			ganadores++
			if r.version != 1 {
				t.Fatalf("el ganador debe dejar la versión en 1, dejó %d", r.version)
			}
		case esConflicto(r.err):
			conflictos++
		default:
			t.Fatalf("error inesperado (ni éxito ni conflicto): %v", r.err)
		}
	}
	if ganadores != 1 || conflictos != 1 {
		t.Fatalf("se esperaba 1 ganador y 1 conflicto; hubo %d y %d", ganadores, conflictos)
	}
}

// TestNoSeAgreganNotasAUnCasoCerrado afirma que, tras el ciclo
// reclamar→resolver→cerrar, agregar una nota devuelve ErrCerrada (ADR 21 §4).
func TestNoSeAgreganNotasAUnCasoCerrado(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, teardown := setup(t, ctx)
	defer teardown()

	caso := activar(t, ctx, svc, activacn1)
	id := caso.EmergenciaID

	// Una nota en abierta sí se acepta.
	if _, err := svc.AgregarNota(ctx, emergencias.EntradaNota{
		TenantID: tenant, EmergenciaID: id, ActorReal: cliente, AudienciaAutor: "Cliente", Cuerpo: "ayuda",
	}); err != nil {
		t.Fatalf("nota en abierta: %v", err)
	}

	// Ciclo hasta cerrada.
	if _, err := svc.Reclamar(ctx, tenant, id, actorA, 0); err != nil {
		t.Fatalf("reclamar: %v", err)
	}
	if err := svc.Resolver(ctx, tenant, id, actorA, 1); err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if err := svc.Cerrar(ctx, tenant, id, actorA); err != nil {
		t.Fatalf("cerrar: %v", err)
	}

	// En cerrada, la nota se rechaza.
	_, err := svc.AgregarNota(ctx, emergencias.EntradaNota{
		TenantID: tenant, EmergenciaID: id, ActorReal: cliente, AudienciaAutor: "Cliente", Cuerpo: "tarde",
	})
	if !errors.Is(err, emergencias.ErrCerrada) {
		t.Fatalf("nota en cerrada: quiere ErrCerrada, fue %v", err)
	}
}

// TestResolverExigeEnAtencion afirma que resolver un caso que sigue en abierta
// (sin reclamo) devuelve ErrTransicionInvalida, y que cerrar un caso no resuelto
// también.
func TestResolverExigeEnAtencion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, teardown := setup(t, ctx)
	defer teardown()

	caso := activar(t, ctx, svc, activacn1)
	id := caso.EmergenciaID

	if err := svc.Resolver(ctx, tenant, id, actorA, 0); !errors.Is(err, emergencias.ErrTransicionInvalida) {
		t.Fatalf("resolver en abierta: quiere ErrTransicionInvalida, fue %v", err)
	}
	if err := svc.Cerrar(ctx, tenant, id, actorA); !errors.Is(err, emergencias.ErrTransicionInvalida) {
		t.Fatalf("cerrar sin resolver: quiere ErrTransicionInvalida, fue %v", err)
	}
}

func esConflicto(err error) bool {
	var c emergencias.ErrConflictoVersion
	return errors.As(err, &c)
}

// TestDobleTransferenciaVersionMonotonica cubre P20 en el caso de emergencia: la
// versión del reclamo avanza en cada transferencia A→B→A (1, 2, 3) en lugar de
// reabrir en 1, así que una versión previa da conflicto en transferir, liberar,
// resolver y reclamar aunque A vuelva a ser responsable. Liberar deja el caso en
// la versión vigente y el siguiente reclamo desde el pool la avanza.
func TestDobleTransferenciaVersionMonotonica(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, teardown := setup(t, ctx)
	defer teardown()
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	id := activar(t, ctx, svc, activacn1).EmergenciaID
	versionCaso := func() int64 {
		t.Helper()
		casos, err := svc.ListarPorViaje(ctx, tenant, viaje, false)
		if err != nil || len(casos) != 1 {
			t.Fatalf("listar: %d casos, err %v", len(casos), err)
		}
		return int64(casos[0].VersionReclamo)
	}
	reclamoActivo := func() (string, string) {
		t.Helper()
		var rid, actor string
		if err := admin.QueryRow(ctx,
			`SELECT id::text, actor_real::text FROM mensajeria.reclamo_emergencia WHERE cerrado_en IS NULL`).Scan(&rid, &actor); err != nil {
			t.Fatalf("leer reclamo activo: %v", err)
		}
		return rid, actor
	}

	if _, err := svc.Reclamar(ctx, tenant, id, actorA, 0); err != nil {
		t.Fatalf("A reclama: %v", err)
	}
	v1 := versionCaso()
	if err := svc.Transferir(ctx, tenant, id, actorA, actorB, 1); err != nil {
		t.Fatalf("A→B: %v", err)
	}
	v2 := versionCaso()
	if err := svc.Transferir(ctx, tenant, id, actorB, actorA, 2); err != nil {
		t.Fatalf("B→A: %v", err)
	}
	v3 := versionCaso()
	if got := [3]int64{v1, v2, v3}; got != [3]int64{1, 2, 3} {
		t.Fatalf("versiones A→B→A: %v, quiere [1 2 3]", got)
	}

	rid, _ := reclamoActivo()
	for paso, err := range map[string]error{
		"transferir con la versión previa": svc.Transferir(ctx, tenant, id, actorA, actorB, 1),
		"liberar con la versión previa":    svc.Liberar(ctx, tenant, id, rid, actorA, 1),
		"resolver con la versión previa":   svc.Resolver(ctx, tenant, id, actorA, 1),
		"reclamar con la versión de B":     func() error { _, e := svc.Reclamar(ctx, tenant, id, actorB, 2); return e }(),
	} {
		if !esConflicto(err) {
			t.Fatalf("%s: quiere conflicto, fue %v", paso, err)
		}
	}
	if _, actor := reclamoActivo(); actor != actorA || versionCaso() != 3 {
		t.Fatalf("tras los conflictos: responsable %s versión %d, quiere %s 3", actor, versionCaso(), actorA)
	}

	// El evento lleva la versión nueva de cada transferencia (contrato
	// EmergenciaTransferida.version_reclamo).
	var versiones []int64
	rows, err := admin.Query(ctx,
		`SELECT (payload->>'version_reclamo')::bigint FROM mensajeria.outbox
		  WHERE tipo = 'emergencia.transferida' AND agregado_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatalf("leer outbox: %v", err)
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("escanear outbox: %v", err)
		}
		versiones = append(versiones, v)
	}
	if len(versiones) != 2 || versiones[0] != 2 || versiones[1] != 3 {
		t.Fatalf("version_reclamo de emergencia.transferida: %v, quiere [2 3]", versiones)
	}

	// Liberar deja el caso en 3; del pool, la versión 0 es obsoleta y la 3 abre en 4.
	if err := svc.Liberar(ctx, tenant, id, rid, actorA, 3); err != nil {
		t.Fatalf("liberar: %v", err)
	}
	if v := versionCaso(); v != 3 {
		t.Fatalf("tras liberar: versión %d, quiere 3", v)
	}
	if _, err := svc.Reclamar(ctx, tenant, id, actorB, 0); !esConflicto(err) {
		t.Fatalf("reclamar del pool con 0: quiere conflicto, fue %v", err)
	}
	if v, err := svc.Reclamar(ctx, tenant, id, actorB, 3); err != nil || v != 4 {
		t.Fatalf("reclamar del pool con 3: versión %d err %v, quiere 4", v, err)
	}
}

// TestListarVistaMinimaExcluyeCerradas afirma que, con soloActivos=true (vista
// mínima del cliente), un caso cerrado no aparece, mientras que con
// soloActivos=false (interno) sí.
func TestListarVistaMinimaExcluyeCerradas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, teardown := setup(t, ctx)
	defer teardown()

	// Un caso que llevamos hasta cerrada.
	cerrado := activar(t, ctx, svc, activacn1)
	if _, err := svc.Reclamar(ctx, tenant, cerrado.EmergenciaID, actorA, 0); err != nil {
		t.Fatalf("reclamar: %v", err)
	}
	if err := svc.Resolver(ctx, tenant, cerrado.EmergenciaID, actorA, 1); err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if err := svc.Cerrar(ctx, tenant, cerrado.EmergenciaID, actorA); err != nil {
		t.Fatalf("cerrar: %v", err)
	}
	// Un caso que sigue abierto.
	abierto := activar(t, ctx, svc, activacn2)

	// Vista mínima (cliente): solo el abierto.
	minima, err := svc.ListarPorViaje(ctx, tenant, viaje, true)
	if err != nil {
		t.Fatalf("listar vista mínima: %v", err)
	}
	if len(minima) != 1 || minima[0].ID != abierto.EmergenciaID {
		t.Fatalf("vista mínima debe traer solo el caso abierto, trajo %d", len(minima))
	}

	// Interno: ambos.
	todos, err := svc.ListarPorViaje(ctx, tenant, viaje, false)
	if err != nil {
		t.Fatalf("listar interno: %v", err)
	}
	if len(todos) != 2 {
		t.Fatalf("interno debe traer los dos casos, trajo %d", len(todos))
	}
}
