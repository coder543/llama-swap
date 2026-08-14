package server

// capacityTotal is zero unless the active router uses the capacity planner.
// The planner itself is unitless; the private UI presents its values as GB.
func (s *Server) capacityTotal() int {
	if s.cfg.Routing.Router.Use != "matrix" {
		return 0
	}
	matrix := s.cfg.Routing.Router.Settings.Matrix
	if !matrix.CapacityMode() {
		return 0
	}
	return matrix.Capacity
}
