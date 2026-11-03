package emergencias_test

import (
	"context"
	"errors"
	"slices"
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
// ganador (versión 1) y un conflicto, y los dos intentos quedan auditados.
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

	// Los dos intentos quedan auditados, sin conversación (DEC-125).
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	var ganados, perdidos, sinCaso int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE accion = 'emergencia.reclamo.intento_ganado'),
		        count(*) FILTER (WHERE accion = 'emergencia.reclamo.intento_perdido'
		                           AND detalle->>'version_esperada' = '0' AND detalle->>'version_actual' = '1'),
		        count(*) FILTER (WHERE conversacion_id IS NOT NULL OR detalle->>'caso_emergencia_id' <> $1)
		   FROM mensajeria.auditoria_mensajeria`, caso.EmergenciaID).Scan(&ganados, &perdidos, &sinCaso); err != nil {
		t.Fatalf("leer auditoría: %v", err)
	}
	if ganados != 1 || perdidos != 1 || sinCaso != 0 {
		t.Fatalf("auditoría: %d ganados, %d perdidos (0→1), %d ajenas; quiere 1, 1, 0", ganados, perdidos, sinCaso)
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
		casos, err := svc.ListarPorViaje(ctx, viaje)
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
	if err := svc.Transferir(ctx, tenant, id, actorA, actorB, 1, false); err != nil {
		t.Fatalf("A→B: %v", err)
	}
	v2 := versionCaso()
	if err := svc.Transferir(ctx, tenant, id, actorB, actorA, 2, false); err != nil {
		t.Fatalf("B→A: %v", err)
	}
	v3 := versionCaso()
	if got := [3]int64{v1, v2, v3}; got != [3]int64{1, 2, 3} {
		t.Fatalf("versiones A→B→A: %v, quiere [1 2 3]", got)
	}

	rid, _ := reclamoActivo()
	for paso, err := range map[string]error{
		"transferir con la versión previa": svc.Transferir(ctx, tenant, id, actorA, actorB, 1, false),
		"liberar con la versión previa":    svc.Liberar(ctx, tenant, id, rid, actorA, 1, false),
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
	if err := svc.Liberar(ctx, tenant, id, rid, actorA, 3, false); err != nil {
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

// TestBandejaUnicaSinTenant afirma la bandeja única de emergencias (DEC-130) sin
// acotar por tenant (DEC-129): casos de dos tenants entran a la MISMA bandeja (la
// más antigua de tipo emergencias), la lectura excluye los cerrados y ordena por
// activada_en con desempate por id (DEC-121 #3). ListarPorViaje devuelve todos,
// también el cerrado.
func TestBandejaUnicaSinTenant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, teardown := setup(t, ctx)
	defer teardown()
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	activarEn := func(tenant, activacion, id, activadaEn string) {
		t.Helper()
		res, err := svc.Activar(ctx, emergencias.EntradaActivacion{
			TenantID: tenant, ViajeID: viaje, ActorReal: actorA,
			Origen: msgclient.OrigenActivacionCliente, ActivacionID: activacion,
		})
		if err != nil {
			t.Fatalf("activar en %s: %v", tenant, err)
		}
		// Ids y horas literales para afirmar el orden; el caso aún no tiene notas
		// ni reclamos que lo referencien.
		dbtest.Exec(t, ctx, admin, `UPDATE mensajeria.emergencia SET id = '`+id+`', activada_en = '`+activadaEn+`'
			 WHERE id = '`+res.EmergenciaID+`'`)
	}
	const (
		e1 = "e1000000-0000-0000-0000-000000000001"
		e2 = "e2000000-0000-0000-0000-000000000002"
		e3 = "e3000000-0000-0000-0000-000000000003"
		e4 = "e4000000-0000-0000-0000-000000000004"
	)
	activarEn("t1", "a1000000-0000-0000-0000-000000000001", e1, "2026-01-02T00:00:00Z")
	activarEn("t2", "a2000000-0000-0000-0000-000000000002", e3, "2026-01-01T00:00:00Z")
	activarEn("t1", "a3000000-0000-0000-0000-000000000003", e2, "2026-01-01T00:00:00Z")
	activarEn("t2", "a4000000-0000-0000-0000-000000000004", e4, "2025-12-31T00:00:00Z")
	dbtest.Exec(t, ctx, admin, `UPDATE mensajeria.emergencia SET estado = 'cerrada' WHERE id = '`+e4+`'`)

	var bandejas []string
	rows, err := admin.Query(ctx, `SELECT DISTINCT bandeja_id::text FROM mensajeria.emergencia`)
	if err != nil {
		t.Fatalf("leer bandejas de los casos: %v", err)
	}
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatalf("escanear: %v", err)
		}
		bandejas = append(bandejas, b)
	}
	if len(bandejas) != 1 || bandejas[0] != bandeja {
		t.Fatalf("bandejas de los casos = %v, quiere solo %s", bandejas, bandeja)
	}

	casos, err := svc.Bandeja(ctx)
	if err != nil {
		t.Fatalf("Bandeja: %v", err)
	}
	var ids []string
	for _, c := range casos {
		ids = append(ids, c.ID)
	}
	if want := []string{e2, e3, e1}; !slices.Equal(ids, want) {
		t.Fatalf("bandeja = %v, quiere %v", ids, want)
	}

	todos, err := svc.ListarPorViaje(ctx, viaje)
	if err != nil {
		t.Fatalf("ListarPorViaje: %v", err)
	}
	if len(todos) != 4 {
		t.Fatalf("ListarPorViaje debe traer los 4 casos de ambos tenants, trajo %d", len(todos))
	}
}

// TestActivarCreaLaBandejaUnica afirma que sin bandeja de emergencias la primera
// activación la crea en su misma tx y la segunda, de otro tenant, la reutiliza
// (DEC-130 #1): queda exactamente una.
func TestActivarCreaLaBandejaUnica(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, teardown := setup(t, ctx)
	defer teardown()
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Exec(t, ctx, admin, `DELETE FROM mensajeria.bandeja`)

	for i, tn := range []string{"t1", "t2"} {
		if _, err := svc.Activar(ctx, emergencias.EntradaActivacion{
			TenantID: tn, ViajeID: viaje, ActorReal: actorA,
			Origen: msgclient.OrigenActivacionOperacion, ActivacionID: []string{activacn1, activacn2}[i],
		}); err != nil {
			t.Fatalf("activar en %s: %v", tn, err)
		}
	}
	var bandejas, enOtra int
	var tenantBandeja string
	if err := admin.QueryRow(ctx,
		`SELECT count(*), min(tenant_id) FROM mensajeria.bandeja WHERE tipo = 'emergencias'`,
	).Scan(&bandejas, &tenantBandeja); err != nil {
		t.Fatalf("contar bandejas: %v", err)
	}
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM mensajeria.emergencia e
		  WHERE NOT EXISTS (SELECT 1 FROM mensajeria.bandeja b WHERE b.id = e.bandeja_id AND b.tipo = 'emergencias')`,
	).Scan(&enOtra); err != nil {
		t.Fatalf("contar casos fuera de la bandeja: %v", err)
	}
	if bandejas != 1 || tenantBandeja != "t1" || enOtra != 0 {
		t.Fatalf("bandejas de emergencias = %d (tenant %q), casos fuera de ella = %d; quiere 1 (t1), 0", bandejas, tenantBandeja, enOtra)
	}
}

// TestResolverSoloElResponsable afirma que con la versión vigente un miembro de
// Reacción que no es el responsable no resuelve (ADR 21 §5) y el caso sigue en
// atención.
func TestResolverSoloElResponsable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, teardown := setup(t, ctx)
	defer teardown()

	id := activar(t, ctx, svc, activacn1).EmergenciaID
	if _, err := svc.Reclamar(ctx, tenant, id, actorA, 0); err != nil {
		t.Fatalf("A reclama: %v", err)
	}
	if err := svc.Resolver(ctx, tenant, id, actorB, 1); !errors.Is(err, emergencias.ErrNoEsResponsable) {
		t.Fatalf("B resuelve el caso de A: %v, quiere ErrNoEsResponsable", err)
	}
	caso, err := svc.Obtener(ctx, tenant, id)
	if err != nil {
		t.Fatalf("Obtener: %v", err)
	}
	if caso.Estado != "en_atencion" || caso.Reclamo == nil || caso.Reclamo.Responsable != actorA || caso.Reclamo.Version != 1 {
		t.Fatalf("caso tras el intento: estado %q reclamo %+v; quiere en_atencion con A en versión 1", caso.Estado, caso.Reclamo)
	}
	if err := svc.Resolver(ctx, tenant, id, actorA, 1); err != nil {
		t.Fatalf("A resuelve: %v", err)
	}
}
