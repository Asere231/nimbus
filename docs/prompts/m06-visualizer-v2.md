# Milestone 6: visualizer v2

## Goal

Turn the visualizer from a live picture into a learning and portfolio tool: replay what happened, see what will change, look inside clusters, and export diagrams.

## Build

1. **Apply timeline.** Group events into "runs" (a burst of writes separated by more than ~5 s of quiet, or tagged by a `nimbus run` wrapper). A scrubber replays a run step by step: resources appear in order, the current API call is highlighted. Store events in the persisted state so runs survive restarts.
2. **Plan view.** `nimbus plan-view` reads `terraform show -json plan.out` (written by `terraform plan -out plan.out`) and shows creates, updates and deletes as colour-coded overlays on the current graph before anything is applied.
3. **Kubernetes view.** A tab per cluster: namespaces → deployments → replica sets → pods, services wired to pods by label selector, with pod phase and restarts. Uses the polling from Milestone 3.
4. **Network view.** VNets as containers with subnets inside, NSGs and route tables attached to subnets, address ranges shown; peering drawn as links.
5. **Export.** Download the current graph as SVG and PNG with a clean, light theme suitable for a README or slide.
6. **Code structure.** Split `app.js` into ES modules (`<script type="module">`), still no build step, no CDN. Add a small test page or headless test that loads the app against a fixture and checks it renders.

## Ask the owner

- Show me 2–3 layout sketches (ASCII or a static HTML mock) for the timeline and Kubernetes views before building them.

## Done when

- [ ] Replaying the `azure-app` apply from Milestone 4 shows each resource appear in dependency order.
- [ ] Plan view shows a pending change to a VNet tag before apply.
- [ ] The Kubernetes view shows the nginx deployment's pods behind its service.
- [ ] An exported SVG of the `azure-app` topology opens cleanly in a browser and is under 200 KB.
- [ ] Works in Chrome and Safari, light and dark mode, at 1280 px and at phone width.
