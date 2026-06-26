package di

// Seal freezes the container and validates the dependency graph.
// After Seal, no more Provide, ProvideFactory, ProvideValue, Replace, Alias, or
// BindMany calls are allowed.
// Seal is idempotent — subsequent calls return the same result.
//
// Seal is side-effect-free: it does not instantiate any singletons
// or perform I/O. It only freezes the container and runs validation.
//
// Resolve is allowed both before and after Seal. Before Seal,
// Resolve works during bootstrap (e.g. ensureRegistry pattern).
// After a failed Seal, Resolve returns the seal error.
// app.Run() calls Seal implicitly via credo.App.Finalize.
func (c *Container) Seal() error {
	c.sealOnce.Do(c.doSeal)
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sealErr
}

// doSeal performs the actual freeze + validate. It runs exactly once via
// sealOnce. sealErr is written under the lock so concurrent resolve readers
// (which read it under RLock) never race with this write.
func (c *Container) doSeal() {
	c.mu.Lock()
	c.frozen = true
	c.mu.Unlock()

	err := c.validate()

	c.mu.Lock()
	c.sealErr = err
	c.mu.Unlock()
}
