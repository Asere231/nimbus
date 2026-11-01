// The three one-liners every DOM module needs.

export const $ = (sel) => document.querySelector(sel);

export const svgNS = "http://www.w3.org/2000/svg";

// el creates an SVG element with attributes and optionally appends it.
export function el(name, attrs, parent) {
  const e = document.createElementNS(svgNS, name);
  for (const k in attrs) e.setAttribute(k, attrs[k]);
  if (parent) parent.appendChild(e);
  return e;
}

// SVG text does not wrap or ellipsise, so labels are truncated by hand.
export function fit(text, max) {
  return text.length > max ? text.slice(0, max - 1) + "…" : text;
}

export function switchTab(name) {
  document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t.dataset.tab === name));
  document.querySelectorAll(".panel").forEach((p) => p.classList.toggle("active", p.id === "panel-" + name));
}
