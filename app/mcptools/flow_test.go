package mcptools

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// En America/Santiago el 2026-09-06 empieza el horario de verano a las
// 00:00: ese día no tiene 00:00 y arranca a la 01:00.
func TestFechasConCambioDeHorario(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skip("sin tzdata:", err)
	}
	to, err := dayArg(map[string]any{"to": "2026-09-05"}, "to", loc, 1)
	if err != nil {
		t.Fatal(err)
	}
	if to.In(loc).Format(time.DateOnly) != "2026-09-06" || !to.Equal(startOfDay(*to, loc)) {
		t.Errorf("fin exclusivo = %v, quiero el primer instante del 2026-09-06", to.In(loc))
	}
	if got := lastDay(timestamppb.New(*to), loc); got != "2026-09-05" {
		t.Errorf("último día = %s, quiero 2026-09-05", got)
	}
	from, _ := dayArg(map[string]any{"from": "2026-09-06"}, "from", loc, 1)
	if from.In(loc).Hour() != 0 {
		t.Errorf("el día después del cambio = %v, quiero las 00:00", from.In(loc))
	}
}
