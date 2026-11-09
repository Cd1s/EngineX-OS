# Auto-analyze: unavailable until a bounded native adapter is verified

The `autoAnalyze`/`auto_analyze` preference still loads, saves and appears in
Library settings. The row explicitly says **Unavailable** in all three UI
languages. `scAutoAnalyzeAvailable` is read-only and false. Enabling the saved
preference does not submit any work, even after inserting or changing drives.

Previously `scAutoAnalyzeTimer` set the native
`/Client/Librarian/Analyzer/AnalyzeUnanalyzed` translator state to true after
15 seconds. That action scans a whole library; it cannot enforce per-file
existence checks, exclude `/data/inMusicTempFiles` copies, cap outstanding
work at one track, or defer new submissions during playback. The timer and
its drive/preference event handlers have been removed. There is no batch
fallback, candidate query, native payload construction, database access or
automatic stop command in this implementation.

The native strings `AddTrackToAnalysisQueue(ITrackDataPtr)`, `StopAnalysis`,
`TracksInProgress` and `AnalysisIsRunning` do not establish a QML-callable
method, the representation/lifetime of `ITrackDataPtr`, the meaning of a
translator's state, or reliable completion acknowledgements. In the tracked
5.0.4 evidence, `docs/re-report/rea-evidence/observations.jsonl` line 94
records only `search_strings` results for AddTrackToAnalysisQueue and
StopAnalysis (addresses 0x1d88700 and 0x1d860b4). This is not payload proof,
and is not proof of compatibility with the target Engine build.

Consequently **no functional per-track filtering/scheduling is claimed**.
Zero submissions prevents autoAnalyze from adding missing paths, temporary
copies, parallel jobs or playback-time jobs. It does not cancel previously
queued work or control Engine's own/manual analysis. Neither `m.db` nor
`stm.db` is opened or written by this change. Hook, affinity, scheduling and
shadow display behavior are outside this patch.

Before re-enabling automatic submission, a replacement must demonstrate:

1. A native-supported per-track payload and its ownership/lifetime for each
   supported Engine version, with acknowledgement and completion evidence.
2. Read-only candidate discovery. Immediately before enqueue, reject missing
   or non-regular files, resolution/stat errors and both lexical and resolved
   paths equal to or beneath `/data/inMusicTempFiles`. Test symlink aliases,
   `..`, broken links, mount changes and disappearance between query/enqueue.
3. At most one outstanding task, including the submission/acknowledgement
   interval. Unknown, stale or busy analysis state must block submissions;
   do not clear an outstanding reservation on a timeout and submit again.
4. Known stopped state for every playback source immediately before enqueue.
   Playing or unknown playback state must block submissions. Toggle/drive
   changes and restart must not bypass the outstanding-task guard.
5. No Engine database writes and no unbounded batch fallback on any error.

Regression checks in `tools/az01-packer/test_ui_mod.py` protect the absence of
native Analyzer calls and automatic trigger handlers, the retained preference,
localized unavailable label and updated release gate. The render probe checks
the declared unavailable capability; it does not prove native queue behavior.

Rollback: reverse the delivered patch against its original base. Restoring the
old timer restores the unsafe whole-library scan and requires a separate
decision. Existing saved preferences are unchanged by this patch.
