package di

import "reflect"

// RegistrationCount returns the number of registered services.
// Exported for testing only.
func (c *Container) RegistrationCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.registrations)
}

// SingletonCount returns the number of built singletons.
// Exported for testing only.
func (c *Container) SingletonCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	count := 0
	for _, entry := range c.singletons {
		if entry.state == entryBuilt {
			count++
		}
	}
	return count
}

// HasRegistration checks whether a type is registered.
// Exported for testing only.
func (c *Container) HasRegistration[T any]() bool {
	t := reflect.TypeFor[T]()
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.registrations[t]
	return ok
}

// IsProtected reports whether T's binding rejects Replace.
// Exported for testing only.
func (c *Container) IsProtected[T any]() bool {
	t := reflect.TypeFor[T]()
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.protected[t]
	return ok
}
