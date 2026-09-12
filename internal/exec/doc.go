// Package exec runs interactive console sessions in Pod containers on the
// gateway's request. It is the third gate on the exec path -- after
// Kubernetes RBAC (pods/exec create, granted by the chart's rbac.exec) and
// alongside the owner's exec.pod rules -- and it owns the session's
// lifetime: the SPDY stream to pods/exec, a ring buffer of recent output for
// replay, the resume window a dropped transport gets, and the hard stop.
//
// Bytes never ride the Connect stream. attach.go dials ExecSession on its
// own gRPC connection; manager.go and session.go know nothing about gRPC.
package exec
