package di

// Seal freezes the container and validates the dependency graph.
// After Seal, no more Provide, ProvideValue, ProvideProtectedValue,
// ProtectBinding, AdoptValue, Replace, Alias, or BindMany calls are allowed.
// Seal is idempotent — subsequent calls return the same result.
//
// Seal is side-effect-free: it does not instantiate any singletons
// or perform I/O. It only freezes the container and runs validation.
//
// Resolve is admitted only after Seal: constructor execution starts once the
// graph is validated, and registration-phase reads of prebuilt values go
// through AdoptValue instead. After a failed Seal, Resolve returns the seal
// error. app.Run() calls Seal implicitly via credo.App.Finalize.
func (c *Container) Seal() error {
	c.sealOnce.Do(c.doSeal)
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sealErr
}

// doSeal performs the actual freeze + validate. It runs exactly once via
// sealOnce. sealErr and sealed are written under the lock so concurrent
// resolve readers (which read them under RLock) never race with the write.
func (c *Container) doSeal() {
	c.mu.Lock()
	c.frozen = true
	c.mu.Unlock()

	err := c.validate()

	c.mu.Lock()
	c.sealErr = err
	c.sealed = true
	c.mu.Unlock()
}
