# Pitch fader pickup (#181)

Select **Pitch Fader Pickup** under User Profile → Playback or in the web console.
The controller polls for changes, which can take up to 10 seconds to apply.

- **Pioneer (Single-Pass)** is the default. The physical fader picks up when it
  crosses or approaches the current pitch. Moving into the center detent unlocks
  and sets zero when the current pitch is close to zero.
- **Direct (Instant)** immediately applies the physical fader position.
- **Stock** uses Engine's original takeover behavior.

The setting persists in `settings.conf` as `pitch_pickup=direct` or
`pitch_pickup=stock`; an absent key selects Pioneer. It affects speed targets
only, leaving other controls' takeover behavior unchanged.
