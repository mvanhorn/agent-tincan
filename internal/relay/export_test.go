package relay

// KeyForTest exposes the relay key to tests.
func KeyForTest(s *Server) string { return s.key }
