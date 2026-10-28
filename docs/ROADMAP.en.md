# LiveGuard Improvement Roadmap

Last reviewed: 2026-10-27

This document collects known problems and possible improvements so collaborators can discuss work, split it into tasks, and agree on evidence. The items below are **candidates**. They are not scheduled or promised, and every item is currently “Candidate — discussion needed.”

[English](ROADMAP.en.md) | [简体中文](ROADMAP.md)

## How to maintain this roadmap together

Each item describes four things: the current problem, what code or measurements prove, a possible direction, and how to tell whether a change worked. Before implementation, collaborators can add alternatives, dependencies, and effort estimates. Update the status after a direction is agreed.

| Status | Meaning |
|---|---|
| Candidate | There is evidence of a problem worth discussing; no decision has been made about whether or when to do it. |
| Confirmed | The direction and acceptance evidence are agreed, but work may not have started. |
| In progress | Someone has taken ownership and implementation has started; add an owner and relevant branch or PR. |
| Complete | The change is merged and the listed tests or measurements have been obtained. |
| Deferred | The work is not being done now; record why and what would make it worth discussing again. |

If an item needs several design options or affects multiple modules, write a focused **RFC (request for comments)** first. Capture the context, candidate designs, trade-offs, and open questions. Once a lasting technical decision has been made, write an **ADR (architecture decision record)** if useful. The roadmap answers “what is worth improving”; it does not replace a design document, task list, or decision record.

## Candidate improvements

### 1. Measure how many target events the capture pipeline can see

**Problem:** LiveGuard captures frames at intervals and recognizes text from them. A comment that appears and disappears between two frames can be missed, and content outside the browser viewport is not visible. Capture and OCR already run separately. The UI can show frame counts, actual intervals, capture time, and frames skipped due to OCR backlog. These metrics can reveal slow processing, but they cannot tell us how many target events were missed.

**Evidence available:** There is no real livestream missed-event rate measured against known ground truth. Synthetic inputs and fixed-image tests can verify code behavior, but cannot prove coverage on real streams.

**Possible direction:** Define how a “target event” is labeled, then build replay samples with timestamps. For real clips, use only small samples with clear permission for project evaluation. Report the sample source and scope, detections, missed cases, time from event to alert, skipped captures, and OCR backlog—not only successful examples.

**Evidence of completion:** Another person can reproduce the evaluation from the instructions. The report states data source, sample count, and limitations, and shows which events were missed and at which processing stage.

**Status:** Candidate — discussion needed.

### 2. Define retention and backup for long-running observation data

**Problem:** The default SQLite mode keeps the full observation history in the `observations` table in `runtime/liveguard.sqlite`. With the current “keep the ledger” behavior, observations are not automatically removed, so disk use grows during long-running sessions. Manual JSON exports also use additional disk space.

**Important boundary:** Screenshots used by the current investigation are held in temporary files. `save_frame_evidence` means that an image is kept temporarily for the current investigation and removed after normal completion. The current path records related text evidence and the text conclusion; it does not archive that screenshot under `runtime/frames`. Cleanup after forced process termination has not been specifically verified. There is still a compatibility path for reading an existing archived image, but that does not mean new screenshots are continuously saved.

**Possible direction:** Show local observation-ledger size and choose a retention policy users can understand, such as manual export, time-based archiving, or deletion after user confirmation. Before adding automatic deletion, define its default, recovery path, and data-loss warning.

**Evidence of completion:** A long synthetic run measures file growth. Backup and restore steps are verified. If cleanup is implemented, tests show exactly what is deleted and that referenced data is not removed by mistake.

**Status:** Candidate — discussion needed.

### 3. Distinguish “matching observations” from “real-world events”

**Problem:** Historical search returns the number of observation records matching text and time filters. OCR may record the same event several times, while different events may use similar wording. Observation count is not event count. The system also has no shared record that an announcement, start, delay, cancellation, and end belong to the same event lifecycle.

**Current boundary:** A single investigation can make at most 6 historical-search calls, each page can return at most 12 items, and one query can include at most 8 candidate phrases. The backend’s `total_matches` is the number of observation records matching the filters, not a deduplicated event count.

**Possible direction:** Add explicit links and state changes for real events that evolve over time, keeping “observation evidence” separate from “event.” First define how states are merged, corrected, or withdrawn; text similarity alone should not cause the model to merge two events.

**Evidence of completion:** Tests cover an announcement followed by a start, a delay, a cancellation, and a correction, as well as similar wording that belongs to different events. Search results clearly distinguish observation count from event count.

**Status:** Candidate — discussion needed.

### 4. Extend load measurements to cover queueing and recovery

**Problem:** Synthetic load tools measure synchronous and asynchronous submission throughput, queue wait, review duration, backpressure, and P95 latency. They use a fixed-latency mock Reviewer. They do not include real browser capture, OCR, disk persistence, network, or model latency, so they are not end-to-end production performance measurements.

**Possible direction:** Add measurements for sustained operation, a full queue, service recovery, and different input rates. Report submission, queue wait, model review, persistence, and final UI visibility as separate stages. Keep real-model results separate from mock results and record environment and API cost.

**Evidence of completion:** The report includes run mode, request count, mock delay, environment, and other parameters; counts accepted, expired, rejected, and failed items; and reports throughput and P50/P95 latency. It includes at least one queue-pressure and recovery scenario, with a clear label for mock versus real dependencies. Keep report definitions aligned with `docs/EVALUATION.md` so results can be compared.

**Status:** Candidate — discussion needed.

### 5. Discuss a per-monitor cumulative model-cost limit

**Problem:** A single investigation has limits on turns, tool calls, and elapsed time, and model requests record token use and latency. There is no hard cumulative token cap per Run or per day. During long-running monitoring, users cannot easily predict the total request cost.

**Possible direction:** Compare showing cumulative usage, warning at a soft threshold, and pausing investigations at a hard threshold. Explain how to display cost when the model cannot estimate it accurately, whether capture continues after reaching the limit, and how the user can resume.

**Evidence of completion:** Users can see cumulative use and understand the limit. Server-side tests cover over-limit behavior. Paused judgment and continued capture are not shown as the same state.

**Status:** Candidate — discussion needed.

### 6. Make failure messages tell users what failed and what to do next

**Problem:** Capture, recognition, queueing, model review, and alert persistence are separate stages. Current states and logs provide some clues, but users may still need to inspect logs to locate a failure or decide whether to sign in again, check configuration, or wait for recovery.

**Possible direction:** For common failures, define four things: stage, user-facing explanation, automatic recovery behavior, and the next user action. Present temporary failures that may recover automatically separately from login or configuration problems that need user action.

**Evidence of completion:** Fault injection or unit tests cover representative errors at each stage. The UI and logs describe the same cause. The operations guide gives actionable recovery steps.

**Status:** Candidate — discussion needed.

## Suggested collaboration order

This is an order for discussion, not an approved schedule:

1. **Establish trustworthy measurements:** make capture-coverage and load reports reproducible before judging later improvements.
2. **Discuss data and event models:** decide observation retention and how to represent real events that start, change, and end.
3. **Improve long-running operation:** use usage and failure evidence to design cost notices, storage management, and recovery guidance.

When taking an item, split it into small, clear tasks. If implementation shows that the problem statement is inaccurate, update the problem and evidence before changing the solution. When complete, add test results, evaluation reports, or the PR link to the item.
