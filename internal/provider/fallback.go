package provider

// The built-in catalog is intentionally empty: providers come from the
// remote catalog (cached in the data dir) and from user config only.
const fallbackCatalog = `{
  "providers": {}
}`
