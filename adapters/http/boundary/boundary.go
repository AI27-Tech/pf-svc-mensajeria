// Package boundary concentra las dos guardas que comparten los adaptadores HTTP
// entrantes de mensajeria y su superficie en proceso: qué claims se aceptan antes
// de tocar SQL y qué parte de un error llega al llamador.
package boundary

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
)

// Claims devuelve los claims del contexto solo si identifican a un actor: nil si
// faltan o si el sujeto no es un UUID canónico (36 caracteres; uuid.Parse también
// acepta urn:uuid: y llaves). El sujeto llega a columnas UUID (usuario_id,
// actor_real), así que un sujeto mal formado se rechaza aquí, igual que la
// ausencia de claims, y nunca llega a Postgres. tenant_id es TEXT y no se valida.
func Claims(ctx context.Context) *auth.Claims {
	c := auth.ClaimsFromContext(ctx)
	if c == nil {
		return nil
	}
	if _, err := uuid.Parse(c.Subject); err != nil || len(c.Subject) != 36 {
		return nil
	}
	return c
}

// WriteError emite el error con la forma Error del contrato y el status de su
// categoría (invariante I3). El cuerpo lleva solo el mensaje público del
// PlatformError, nunca la causa envuelta (Err): esa causa puede traer texto de
// Postgres u otro detalle interno.
func WriteError(w http.ResponseWriter, err error) {
	cat := platformerrors.AsCategory(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cat.HTTPStatus())
	_ = json.NewEncoder(w).Encode(msgclient.Error{
		Codigo:  cat.String(),
		Mensaje: publicMessage(err),
	})
}

// publicMessage es el Message del PlatformError más externo; un error fuera de la
// taxonomía no tiene mensaje público.
func publicMessage(err error) string {
	var pe *platformerrors.PlatformError
	if errors.As(err, &pe) {
		return pe.Message
	}
	return "error interno"
}
