package common

// ControllerName is the field manager in metadata.managedFields on every object
// the controllers write through their client. Without it the field manager is
// derived from the User-Agent, the binary name "manager", so managedFields would
// name the pod rather than the thing that wrote the field. Controller names, the
// "controller" label on metrics and logs, are separate and stay per controller.
const ControllerName = "llm-operator"

// HelmFieldManager is the field manager on everything the in-process Helm SDK
// applies. Without it Helm names itself after the binary, "manager". It differs
// from the "helm" CLI on purpose: a manual helm upgrade of an LLMService release
// then hits a server-side apply conflict naming this manager, a hint that the
// release is operator-managed. Changing it makes every owned release conflict on
// its next upgrade that changes a field.
const HelmFieldManager = "llm-operator-helm"
