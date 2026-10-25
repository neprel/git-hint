package runner

// The single procedure catalogue retains its legacy private variable name.
// Deliberately EMPTY for both preparation and frozen verification purposes.
// Only a separately reviewed source change can install a policy.
// There is no runtime registration API, environment override or public constructor.
// Tests temporarily supply exact private fixture entries; none enters production.
var referencePreparationPolicies = []referencePolicyEntry{}
