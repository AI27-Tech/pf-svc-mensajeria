package conversacion

import (
	"testing"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
)

// Si el contrato agrega un estado de la solicitud, esta prueba falla hasta que se
// decida si cierra la ventana (terminal o VENCIDA) o la abre (estadosSolicitudAbiertos).
func TestEstadosSolicitudCubiertos(t *testing.T) {
	for _, e := range estados.EstadosSolicitud {
		if _, conocido := efectoVentana(string(e)); !conocido {
			t.Errorf("el estado %s del contrato no tiene efecto definido sobre la ventana de la conversación", e)
		}
	}
}

func TestEfectoVentana(t *testing.T) {
	for estado, quiere := range map[string]struct{ cierra, conocido bool }{
		"APROBADO": {true, true}, "VENCIDA": {true, true}, "RECHAZADO_POR_AI27": {true, true},
		"SOLICITADO": {false, true}, "COTIZADO": {false, true}, "PAGADO": {false, true},
		"ESTADO_NUEVO": {false, false}, "": {false, false},
	} {
		if c, k := efectoVentana(estado); c != quiere.cierra || k != quiere.conocido {
			t.Errorf("%q: cierra=%v conocido=%v; quiere %+v", estado, c, k, quiere)
		}
	}
}
