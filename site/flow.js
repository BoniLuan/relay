"use strict";

// Illustrative states only. This page never connects to the Relay API.
const scenarios = {
  delivered: [
    "pending → attempting → succeeded",
    "The receiver returns 204. Relay records success. The response confirms acceptance by the receiver, not completion of its business workflow.",
  ],
  retry: [
    "attempting → retry_wait → attempting → succeeded",
    "Example: a 503 schedules a jittered retry; the next attempt returns 204. The initial round allows up to three starts; a replay requires explicit authorization. The event ID stays the same so receivers can deduplicate.",
  ],
  crash: [
    "attempting → unknown → retry_wait",
    "If a worker disappears after starting HTTP, the outcome is uncertain. Lease recovery preserves an unknown attempt and schedules another only if budget remains. The receiver may already have processed the event.",
  ],
};

for (const button of document.querySelectorAll("[data-scenario]")) {
  button.addEventListener("click", () => {
    const [path, detail] = scenarios[button.dataset.scenario];
    for (const choice of document.querySelectorAll("[data-scenario]")) {
      choice.setAttribute("aria-pressed", String(choice === button));
    }
    document.getElementById("scenario-path").textContent = path;
    document.getElementById("scenario-detail").textContent = detail;
  });
}
