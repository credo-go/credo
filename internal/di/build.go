package di

// Trigger is what finalized the container, which a registration refused
// afterwards names: By is the call, with its site when it has one — such as
// "Resolve[*app.Hub] at /src/main.go:42" — and Remedy the advice that
// follows it, such as "register before that call".
type Trigger struct {
	By     string
	Remedy string
}

// finalizeTrigger is the trigger of a Seal call made without one.
var finalizeTrigger = Trigger{By: "Finalize", Remedy: "register before that call"}

// Seal is SealBy with the trigger "Finalize".
func (c *Container) Seal() error {
	return c.SealBy(finalizeTrigger)
}

// SealBy freezes the container and validates the dependency graph, recording
// t as what finalized it when this call is the one that seals. After it, no
// more Provide, ProvideValue, Manage, Alias, or BindMany calls are allowed,
// and each refusal names t. Sealing is idempotent — subsequent calls return
// the same result and leave the recorded trigger as it was; concurrent
// callers seal once.
//
// SealBy is side-effect-free: it does not instantiate any singletons or
// perform I/O. It freezes the container, drops each type an override of its
// alias left unreached, and runs validation. The first Resolve seals
// implicitly; after a failed seal, Resolve returns the seal error.
func (c *Container) SealBy(t Trigger) error {
	c.sealOnce.Do(func() { c.doSeal(t) })
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sealErr
}

// SealOnResolve seals the container with the trigger trigger returns, unless
// it is sealed already or a shutdown has begun; trigger runs only then. A
// resolving call makes it first, so that the container records the call's
// own site rather than the generic one Resolve records.
func (c *Container) SealOnResolve(trigger func() Trigger) {
	c.mu.RLock()
	skip := c.trigger != nil || c.frozen
	c.mu.RUnlock()
	if !skip {
		_ = c.SealBy(trigger())
	}
}

// doSeal performs the actual freeze + validate. It runs exactly once via
// sealOnce. sealErr and sealed are written under the lock so concurrent
// resolve readers (which read them under RLock) never race with the write.
func (c *Container) doSeal(t Trigger) {
	c.mu.Lock()
	c.frozen = true
	c.trigger = &t
	c.dropUnreachedLocked()
	c.mu.Unlock()

	err := c.validate()

	c.mu.Lock()
	c.sealErr = err
	c.sealed = true
	c.mu.Unlock()
}
