package project

// This core carries only the process-local issuer and closed checkpoint kind.
// Kind-specific wrappers retain their actual typed bindings. It grants no source,
// durable-origin or restart authority and exposes no generic public issuer.
const preworkAcceptanceVersion = "project-prework-acceptance-v1"

type preworkKind uint8

const (
	preworkReferencePreparation preworkKind = iota + 1
	preworkSelectedSource
	preworkOriginalStart
)

// Nonzero storage prevents zero-sized pointer identity aliasing.
type preworkIssuer struct{ nonzero byte }

var preworkProcessIssuer = &preworkIssuer{1}

type preworkAcceptance struct {
	issuer *preworkIssuer
	kind   preworkKind
}

func validPreworkKind(kind preworkKind) bool {
	return kind == preworkReferencePreparation || kind == preworkSelectedSource || kind == preworkOriginalStart
}

// mintPreworkAcceptance performs memory-only issuance. A kind-specific producer
// must finish all success IO/release and its final caller-endpoint check FIRST.
// The reference and selected-source producers use this memory-only core.
func mintPreworkAcceptance(kind preworkKind) preworkAcceptance {
	if !validPreworkKind(kind) {
		return preworkAcceptance{}
	}
	return preworkAcceptance{preworkProcessIssuer, kind}
}
func matchPreworkAcceptance(a preworkAcceptance, kind preworkKind) bool {
	return validPreworkKind(kind) && a.issuer == preworkProcessIssuer && a.kind == kind
}
