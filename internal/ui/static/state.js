// The one piece of mutable state every module shares. Kept in its own module
// so nothing has to import the entry point (which would be a cycle).

export const state = {
  cloud: "azure",
  resources: new Map(), // lowercased id -> resource
  selected: null,
  entered: new Set(),   // ids to animate in on next render
  flashed: new Set(),   // ids to flash on next render
  info: null,
};
