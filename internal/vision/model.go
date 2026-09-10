package vision

const (
	StatusNormal          = "normal"
	StatusSpaghetti       = "spaghetti"
	StatusClog            = "clog"
	StatusObjectDisplaced = "object_displaced"
	StatusNozzleCollision = "nozzle_collision"
	StatusMaterialBuildup = "material_buildup"
	StatusUnknown         = "unknown"
)

type Result struct {
	Status     string  `json:"status"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

func (r Result) Abnormal() bool {
	switch r.Status {
	case StatusSpaghetti, StatusClog, StatusObjectDisplaced, StatusNozzleCollision, StatusMaterialBuildup:
		return true
	default:
		return false
	}
}

func KnownStatus(status string) bool {
	switch status {
	case StatusNormal, StatusSpaghetti, StatusClog, StatusObjectDisplaced, StatusNozzleCollision, StatusMaterialBuildup, StatusUnknown:
		return true
	default:
		return false
	}
}
