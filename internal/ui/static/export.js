// Exporting the current graph as a standalone SVG or PNG, for a README or a
// slide.
//
// The one trap here: the page's styling lives in style.css, and an external
// stylesheet does NOT apply to an SVG that has been detached from the
// document -- not when it is saved to a file, and not when a browser renders
// it inside <img src="data:image/svg+xml,...">. An un-styled export comes out
// as black text on a transparent background with no cards at all. So the
// clone carries its own <style>, written below with light-theme values
// resolved to literals rather than var() tokens, because an exported diagram
// is going into a document that is almost certainly light.
//
// Nothing in the graph uses <foreignObject> and every label is a real <text>
// element, which is why the canvas path works at all: a foreignObject taints
// the canvas in several browsers and would make toBlob throw.

const EXPORT_CSS = `
  .bg { fill: #ffffff; }
  .edge { fill: none; stroke: #c4ccd5; stroke-width: 1.4; }
  .node rect.card { fill: #ffffff; stroke: #c4ccd5; stroke-width: 1.2; }
  .node.state rect.card { stroke: #6e7781; stroke-dasharray: 5 3; }
  .node.modelled rect.card { stroke: #0969da; }
  .node.real rect.card { stroke: #3fb950; }
  .node .badge { font: 600 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  .node .nm { fill: #1f2328; font-weight: 600; font-size: 13px;
    font-family: ui-sans-serif, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; }
  .node .ty { fill: #59636e; font-size: 11.5px;
    font-family: ui-sans-serif, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; }
  .link-identity { fill: none; stroke: #8250df; stroke-opacity: .6; stroke-width: 1.4; stroke-dasharray: 5 4; }
  .net-box { fill: #ffffff; stroke: #c4ccd5; stroke-width: 1.2; }
  .net-title { fill: #1f2328; font-weight: 650; font-size: 14px;
    font-family: ui-sans-serif, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; }
  .net-sub, .net-cidr { fill: #59636e; font-size: 11.5px;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  .net-sub-box { fill: #f6f8fa; stroke: #d8dee4; stroke-width: 1; }
  .net-sub-name { fill: #1f2328; font-weight: 600; font-size: 12.5px;
    font-family: ui-sans-serif, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; }
  .net-chip rect { fill: #fff8e5; stroke: #bf8700; stroke-opacity: .6; }
  .net-chip text { fill: #9a6700; font-size: 10.5px;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  .net-chip.rt rect { fill: #ddf4ff; stroke: #0969da; stroke-opacity: .6; }
  .net-chip.rt text { fill: #0550ae; }
  .net-orphan rect.card { stroke-dasharray: 5 3; }
  .link-peering { fill: none; stroke: #1b7c83; stroke-opacity: .7; stroke-width: 1.6; stroke-dasharray: 7 4; }
  .k8s-cluster-box { fill: #ffffff; stroke: #326ce5; stroke-opacity: .55; stroke-width: 1.4; }
  .k8s-ns-box { fill: #f6f8fa; stroke: #d8dee4; stroke-dasharray: 6 4; }
  .k8s-dep-box { fill: #ffffff; stroke: #c4ccd5; }
  .k8s-rs-box { fill: #ffffff; stroke: #d8dee4; stroke-dasharray: 4 3; }
  .k8s-pod-box, .k8s-node-box { fill: #f6f8fa; stroke: #c4ccd5; }
  .k8s-svc-box { fill: #fff8e5; stroke: #bf8700; stroke-opacity: .7; }
  .k8s-cluster-title, .k8s-dep-title, .k8s-svc-title { fill: #1f2328; font-weight: 650; font-size: 13px;
    font-family: ui-sans-serif, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; }
  .k8s-pod-name, .k8s-node-name { fill: #1f2328; font-size: 12px;
    font-family: ui-sans-serif, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; }
  .k8s-ns-title, .k8s-rs-title, .k8s-status, .k8s-meta { fill: #59636e; font-size: 11px;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  .k8s-sel { fill: #9a6700; font-size: 10.5px;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  .k8s-phase { fill: #59636e; }
  .k8s-pod.phase-running .k8s-phase { fill: #1a7f37; }
  .k8s-pod.phase-pending .k8s-phase { fill: #bf8700; }
  .k8s-pod.phase-failed .k8s-phase { fill: #cf222e; }
  .link-selector { fill: none; stroke: #bf8700; stroke-opacity: .7; stroke-width: 1.4; }
`;

// snapshot clones the live <svg> into something that stands alone: a white
// background, its own stylesheet, and none of the on-screen-only state
// (selection, hover, the enter/flash animations). Nodes the filter has dimmed
// are dropped entirely, so filtering and then exporting gives a clean diagram
// of just the part you were looking at.
export function snapshot(live) {
  const svg = live.cloneNode(true);
  svg.setAttribute("xmlns", "http://www.w3.org/2000/svg");
  svg.setAttribute("xmlns:xlink", "http://www.w3.org/1999/xlink");
  svg.removeAttribute("tabindex");

  svg.querySelectorAll(".dim").forEach((n) => n.remove());
  svg.querySelectorAll("[tabindex]").forEach((n) => n.removeAttribute("tabindex"));
  for (const n of svg.querySelectorAll(".node, .net-vnet, .net-subnet")) {
    n.classList.remove("selected", "enter", "flash", "leaving");
  }

  const vb = (live.getAttribute("viewBox") || "").split(/\s+/).map(Number);
  const w = vb[2] || live.clientWidth, h = vb[3] || live.clientHeight;
  // Export at the diagram's own size, not the on-screen scaled size.
  svg.setAttribute("width", w);
  svg.setAttribute("height", h);

  const style = document.createElementNS("http://www.w3.org/2000/svg", "style");
  style.textContent = EXPORT_CSS;
  const bg = document.createElementNS("http://www.w3.org/2000/svg", "rect");
  bg.setAttribute("class", "bg");
  bg.setAttribute("x", vb[0] || 0);
  bg.setAttribute("y", vb[1] || 0);
  bg.setAttribute("width", w);
  bg.setAttribute("height", h);
  svg.prepend(bg);
  svg.prepend(style);
  return { svg, width: w, height: h };
}

export function svgText(live) {
  const { svg } = snapshot(live);
  return '<?xml version="1.0" encoding="UTF-8"?>\n' + new XMLSerializer().serializeToString(svg);
}

function download(blob, filename) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  // Revoked on the next turn of the event loop: Safari has not finished with
  // the URL when click() returns.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

export function downloadSVG(live, name = "nimbus-topology") {
  download(new Blob([svgText(live)], { type: "image/svg+xml" }), name + ".svg");
}

export function downloadPNG(live, name = "nimbus-topology") {
  const { width, height } = snapshot(live);
  const text = svgText(live);
  const img = new Image();
  // The SVG goes in as a data URL rather than a blob URL: a blob URL counts
  // as a different origin in Safari and taints the canvas, so toBlob throws
  // a SecurityError.
  img.src = "data:image/svg+xml;charset=utf-8," + encodeURIComponent(text);
  img.onload = () => {
    const ratio = 2; // readable when dropped into a README at half size
    const canvas = document.createElement("canvas");
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    const ctx = canvas.getContext("2d");
    ctx.fillStyle = "#ffffff";
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
    ctx.drawImage(img, 0, 0);
    canvas.toBlob((blob) => { if (blob) download(blob, name + ".png"); }, "image/png");
  };
}
