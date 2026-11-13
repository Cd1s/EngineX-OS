"""Regression checks for the shadow scratch review findings (#240, #217)."""
import unittest
import shutil
import test_ui_mod

RX = r"""
const props=new Map();
let posMs=10000, seeks=0, wheelMoves=[], frameOnly=false, mirrorWrites=[];
const MIRROR='/Engine/BeatInfoSenderControl/{l}/PositionInSamples2', MIRROR_ON='/Engine/BeatInfoSenderControl/{l}/Enabled';
globalThis.scShadowBent=null;
globalThis.scShadowBendTimer={interval:0,running:false,stop(){this.running=false},restart(){this.running=true}};
const P=path=>{
    let p=props.get(path);
    if(p)return p;
    // Stock's position mirror reads -1 until its stream runs; the jog moves the deck 1 ms per 20 ticks (measured on a JP07).
    p={_s:path.endsWith('/SongLoaded')||path.endsWith('/PlayState'),_v:path.endsWith('/SampleRate')?48000:path.includes('/PositionInSamples')?-1:0,
       string:path.endsWith('/SpeedRange')?'8':path.endsWith('/SyncMode')?'Off':'',
       entries:['4','8','10','20','50','100'],index:1,
       get state(){assert(!frameOnly,"state read during scratch frame: "+path);return this._s},set state(v){if(path.endsWith('/Enabled'))mirrorWrites.push(v);this._s=v},
       get value(){assert(!frameOnly||path.includes('/PositionInSamples'),"value read during scratch frame: "+path);return this._v},set value(v){this._v=v;if(path.endsWith('/SetSongPosition')){posMs=v*400000;seeks++}},
       get unnormalized(){assert(!frameOnly||path.endsWith('/TrackPosition'),"extra frame read: "+path);return path.endsWith('/TrackLength')?48000*400:path.endsWith('/TrackPosition')?posMs*48:0},
       incValue(t){wheelMoves.push(t);posMs+=t/20},
       decValue(t){wheelMoves.push(-t);posMs-=t/20}};
    props.set(path,p);
    return p;
};
globalThis.scTr=P;
let seq=0;
const E=(o)=>Object.assign({deck:2,path:'/data/enginex/shadow-v3/peer-1/a.flac',id:'1',playing:true,link:'{l}',positionMs:11500,at:now,pitch:0,bpm:128,currentBpm:128,jumps:0,seq:++seq},o);
scShadowApplyAll([E({positionMs:10000})],false);
now+=1500; posMs=11500;
scShadowApplyAll([E({})],false);
for(let i=1;i<=5;i++){now+=25;scShadowApplyAll([E({scratch:true,positionMs:11500-i*40,at:now})],false);scShadowScratchTimer.tick();}
assert.equal(P('/Engine/Deck2/ExternalScratchWheelTouch').state,true,'precondition: touch engaged');
"""

SENDER = r"""
let pos=10000*48, playing=true, touch=false, looping=false;
globalThis.XMLHttpRequest=class {
    constructor(){this.readyState=0;}
    open(m,url){this.url=url;this.readyState=1;}
    send(body){this.readyState=4;this.status=200;}
};
globalThis.scShadowPoll=()=>{};
scFourDeckRole='1';
globalThis.scTr=path=>({
    get value(){return path.endsWith('/SampleRate')?48000:0},
    get unnormalized(){return path.endsWith('/TrackPosition')?pos:path.endsWith('/TrackLength')?48000*400:path.endsWith('BPM')?128:0},
    get state(){return path.endsWith('/SongLoaded')||(path.endsWith('/PlayState')&&playing)||(path.endsWith('/ExternalScratchWheelTouch')&&touch)||(path.endsWith('/Track/LoopEnableState')&&looping)},
    get string(){return path.endsWith('/TrackUri')?'file:///a.mp3':''}, color:'#ffffff'});
for(let i=0;i<30;i++){now+=80;pos+=80*48;scShadowTick();}
"""


@unittest.skipUnless(shutil.which("node"), "node needed for scratch regressions")
class ShadowScratchReviewTest(unittest.TestCase):
    js = test_ui_mod.ShadowDisplayTest.js

    def test_touch_released_when_source_loads_another_track_mid_scratch(self):
        self.js(RX + r"""
now+=25;
scShadowApplyAll([E({id:'2',path:'/data/enginex/shadow-v3/peer-2/b.flac',scratch:false,positionMs:0,at:now})],false);
now+=1500;
scShadowApplyAll([E({id:'2',path:'/data/enginex/shadow-v3/peer-2/b.flac',scratch:false,positionMs:1500,at:now})],false);
assert.equal(P('/Engine/Deck2/ExternalScratchWheelTouch').state,false,'touch still held on the shadow deck after the track changed');
""")

    def test_scratch_enable_is_restored_after_release(self):
        self.js(RX + r"""
now+=25;
scShadowApplyAll([E({scratch:false,positionMs:11330,at:now})],false);
assert.equal(P('/Engine/Deck2/ExternalScratchWheelTouch').state,false);
assert.equal(P('/Engine/Deck2/ExternalScratchWheelScratchEnable').state,false,'ScratchEnable left on after release');
""")

    def test_touch_released_when_source_disappears(self):
        self.js(RX + r"""
now+=25;
scShadowApplyAll([],false);
assert.equal(P('/Engine/Deck2/ExternalScratchWheelTouch').state,false,'touch still held after the source vanished');
""")

    def test_touch_released_on_switch_to_display_mode(self):
        self.js(RX + r"""
now+=25;
scShadowApplyAll([E({displayOnly:true,scratch:true,positionMs:11300,at:now})],true);
assert.equal(P('/Engine/Deck2/ExternalScratchWheelTouch').state,false,'touch still held after switching to display mode');
""")

    def test_touch_released_if_pause_and_release_arrive_together(self):
        self.js(RX + r"""
now+=25;
scShadowApplyAll([E({playing:false,scratch:false,positionMs:11330,at:now})],false);
now+=25;
scShadowApplyAll([E({playing:false,scratch:false,positionMs:11330,at:now})],false);
assert.equal(P('/Engine/Deck2/ExternalScratchWheelTouch').state,false,'touch held after release+pause');
""")

    def test_sender_does_not_call_a_short_loop_a_scratch(self):
        self.js(SENDER + r"""
looping=true;
const loopStart=10000+30*80, L=125; // 1/4 beat loop at 120 BPM
let t0=now, flagged=0, ticks=0;
for(let i=0;i<60;i++){
    now+=scShadowTickTimer.interval;
    pos=(loopStart+((now-t0)%L))*48;
    scShadowTick(); ticks++;
    if(scShadowPrev[1].scratch) flagged++;
}
console.log('loop: scratch flagged on '+flagged+' of '+ticks+' ticks');
assert.equal(flagged,0,'short loop flagged as scratch on '+flagged+' ticks');
""")

    def test_sender_two_stale_reads_while_playing_are_not_a_scratch(self):
        self.js(SENDER + r"""
let flagged=0;
for(let i=0;i<8;i++){now+=25; pos+=25*48; scShadowTick();}
const frozen=pos;
for(let i=0;i<2;i++){now+=25; pos=frozen; scShadowTick(); if(scShadowPrev[1].scratch) flagged++;}
assert.equal(flagged,0,'two unchanged position reads (50 ms) flagged as scratch');
""")

    def test_sweep_loop_lengths(self):
        self.js(SENDER + r"""
looping=true;
for (const L of [30,60,90,125,180,250,500]) {
  // fresh warm state per length: replay a steady play then loop
  Object.keys(scShadowPrev).forEach(k=>delete scShadowPrev[k]);
  for(let i=0;i<10;i++){now+=80;pos+=80*48;scShadowTick();}
  const start=pos/48, t0=now; let flagged=0, ticks=0;
  for(let i=0;i<60;i++){ now+=scShadowTickTimer.interval; pos=(start+((now-t0)%L))*48; scShadowTick(); ticks++; if(scShadowPrev[1].scratch) flagged++; }
  assert.equal(flagged,0,'loop length '+L+' ms flagged '+flagged+'/'+ticks+' ticks');
}
// Loop Roll and a normal loop share LoopEnableState; touch takes precedence.
touch=true; now+=25; pos-=15*48; scShadowTick();
assert.equal(scShadowPrev[1].scratch,true,'physical scratch inside a loop must still work');
""")

    def test_scratch_enable_preserves_an_original_true_value(self):
        self.js(RX.replace("scShadowApplyAll([E({positionMs:10000})],false);", "P('/Engine/Deck2/ExternalScratchWheelScratchEnable').state=true; scShadowApplyAll([E({positionMs:10000})],false);") + r"""
now+=25; scShadowApplyAll([E({scratch:false})],false);
assert.equal(P('/Engine/Deck2/ExternalScratchWheelScratchEnable').state,true);
""")

    def test_retry_load_and_stale_source_release_scratch(self):
        for action in ["scShadowLoad(2,scShadowAt[2],E({}),now)",
                       "scShadowApplyAll([E({scratch:true,stale:true})],false)"]:
            with self.subTest(action=action):
                self.js(RX + "now+=25; " + action + r""";
assert.equal(P('/Engine/Deck2/ExternalScratchWheelTouch').state,false);
assert.equal(P('/Engine/Deck2/ExternalScratchWheelScratchEnable').state,false);
""")

    def test_fast_scratch_caps_decoder_flushes(self):
        self.js(RX + r"""
const before=seeks;
for(let i=0;i<40;i++) {
    now+=25;
    scShadowApplyAll([E({scratch:true,positionMs:20000+i*250,at:now})],false);
    scShadowScratchTimer.tick();
}
assert(seeks>before,'large position discontinuity did not use the seek fallback');
assert(seeks-before<=4,'fast scratch flushed decoder '+(seeks-before)+' times per second');
""")

    def test_interpolated_small_and_heavy_scratches_move_every_frame(self):
        # Our own updates only (no Engine Link, a CDJ): the frames run one update behind, on the line between the last two.
        for speed in [0.35, 3.5]:
            with self.subTest(speed=speed):
                self.js(RX + "const speed=" + str(speed) + r""";
const start=now, beforeSeeks=seeks;
const position=t=>11300-speed*((t%500)<=250?t%500:500-t%500);
let previousFramePos=posMs, frames=0, maxError=0;
for(let t=1;t<=2000;t++) {
    now=start+t;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:position(t),at:now})],false);
    if(t%16!==0) continue;
    const before=previousFramePos;
    frameOnly=true; scShadowScratchTimer.tick(); frameOnly=false;
    if(t>160) {
        // An update is played out over the 25 ms to the next one, from when it came.
        const expected=position(t-25), phase=(t-25)%250;
        if(phase>40&&phase<210) {
            assert(Math.abs(posMs-before)>0.1,'idle scratch frame between source updates at '+t);
            assert(Math.abs(posMs-before)<=speed*16*1.6+1,'large per-frame jog burst at '+t);
            frames++;
        }
        // Every frame, the turns too: the frame that sees a turn must not set off the new way at full speed.
        maxError=Math.max(maxError,Math.abs(posMs-expected));
    }
    previousFramePos=posMs;
}
assert(frames>60,'insufficient moving-frame coverage');
assert(maxError<=speed*16+2,'the frames are not one update behind the source: '+maxError);
assert.equal(seeks,beforeSeeks,'heavy scratch must never re-buffer the decoder');
// After updates stop: half an update (12.5 ms) more on the line, then at rest there.
for(let t=2016;t<=2320;t+=16) {now=start+t;frameOnly=true;scShadowScratchTimer.tick();frameOnly=false;}
assert(Math.abs(posMs-(position(2000)+speed*12.5))<2,'the deck did not come to rest half an update past the last one: '+(posMs-position(2000)));
now+=16;
scShadowApplyAll([E({scratch:false,positionMs:position(2000),at:now})],false);
assert.equal(scShadowScratchTimer.running,false,'frame timer kept running after release');
assert.equal(P('/Engine/Deck2/ExternalScratchWheelTouch').state,false);
assert(Math.abs(posMs-position(2000))<=35,'scratch-end landing changed');
""")

    # What a JP07 deck does with the jog (measured): 1 ms per 20 ticks from the next audio
    # block on, and a position report 29 times a second that is 20 ms old when it comes.
    PLANT = r"""
let truePos=posMs, pending=0; const trail=[];
const stepPlant=t=>{
    if(t%12===0){truePos+=pending;pending=0}
    trail.push(truePos);
    if(t%35===0) posMs=trail[Math.max(0,trail.length-21)];
};
const frame=()=>{const before=posMs; frameOnly=true; scShadowScratchTimer.tick(); frameOnly=false; pending+=posMs-before; posMs=before;};
"""

    def test_heavy_scratch_on_a_late_reporting_deck_neither_overshoots_nor_turns_back(self):
        # The maintainer's "搓碟还是很卡": steering by the deck's late report at 35 ticks a ms
        # drew a +-160 ms scratch +-800 ms wide, a third of the time the wrong way (two JP07).
        self.js(RX + self.PLANT + r"""
P('/Engine/Deck2/PlayState').state=false;
const start=now, beforeSeeks=seeks, w=2*Math.PI*2.5/1000;
const source=t=>11500+160*(1-Math.cos(w*t)), speed=t=>160*w*Math.sin(w*t);
let wrong=0, moving=0, low=Infinity, high=-Infinity, worst=0, last=truePos;
for(let t=1;t<=3000;t++) {
    now=start+t;
    stepPlant(t);
    if(t%35===0) P(MIRROR).value=source(t)*48;                      // stock's mirror, every third audio block
    if(t%25===0) scShadowApplyAll([E({playing:false,scratch:true,positionMs:source(t-40),at:now})],false); // our own update, 40 ms later
    if(t%16!==0) continue;
    frame();
    if(t>400) {
        const v=speed(t-60), moved=truePos-last;
        if(Math.abs(v)>1) { moving++; if(moved*v<0) wrong++; }
        low=Math.min(low,truePos); high=Math.max(high,truePos);
        worst=Math.max(worst,Math.abs(truePos-source(t-60)));
    }
    last=truePos;
}
console.log('heavy scratch: wrong-way frames '+wrong+'/'+moving+', travel '+low.toFixed(0)+'..'+high.toFixed(0)+' (source 11500..11820), worst error '+worst.toFixed(0)+' ms');
assert(moving>80,'insufficient coverage');
assert.equal(wrong,0,'the deck moved against the source on '+wrong+' of '+moving+' frames');
assert(low>=11500-15&&high<=11820+15,'the deck left the source\'s travel: '+low.toFixed(0)+'..'+high.toFixed(0));
assert(worst<=45,'the deck is not on the source\'s line: '+worst.toFixed(0)+' ms');
assert.equal(seeks,beforeSeeks,'a scratch must never re-buffer the decoder');
""")

    def test_scratch_follows_stock_mirror_not_the_later_update(self):
        self.js(RX + r"""
const start=now;
// The source runs at 2x; our own update says where it was 80 ms ago (160 ms of track): the mirror counts.
for(let t=1;t<=700;t++) {
    now=start+t;
    if(t%35===0) P(MIRROR).value=(11300+2*t)*48;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:11300+2*Math.max(0,t-80),at:now})],false);
    if(t%16===0) { frameOnly=true; scShadowScratchTimer.tick(); frameOnly=false; }
}
assert(Math.abs(posMs-(11300+2*(700-35-16)))<40,'the deck is not one mirror sample behind the source: '+posMs);
// The stream stops (the player went away and came back: -1): our own update takes over.
P(MIRROR).value=-1;
for(let t=701;t<=1400;t++) {
    now=start+t;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:12650,at:now})],false);
    if(t%16===0) { frameOnly=true; scShadowScratchTimer.tick(); frameOnly=false; }
}
// One update took it 110 ms on, then nothing: it rests at most half that step past the update.
assert(posMs>=12650-5&&posMs<=12650+110*0.5+5,'without the mirror the deck did not follow our own update: '+posMs);
""")

    def test_remapped_slot_reads_the_mirror_by_the_source_deck_number(self):
        # #160: a peer's Deck 1 shown on this unit's shadow Deck 2.
        self.js(RX.replace("E=(o)=>Object.assign({deck:2,", "E=(o)=>Object.assign({deck:2,src:1,") + r"""
const start=now;
for(let t=1;t<=700;t++) {
    now=start+t;
    if(t%35===0) P('/Engine/BeatInfoSenderControl/{l}/PositionInSamples1').value=(11300+2*t)*48;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:11300,at:now})],false);
    if(t%16===0) scShadowScratchTimer.tick();
}
assert(posMs>12400,'the mirror of the source\'s own deck number was not read: '+posMs);
""")

    def test_late_mirror_sample_slows_the_glide_but_never_turns_it_back(self):
        self.js(RX + r"""
const start=now; let fed=0, src=11300;
const run=(from,to,feed)=>{ for(let t=from;t<=to;t++) { now=start+t;
    if(t%35===0&&feed(t)) P(MIRROR).value=src*48;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:src,at:now})],false);
    if(t%16===0) { frameOnly=true; scShadowScratchTimer.tick(); frameOnly=false; } } };
run(1,490,t=>{src=11300+2*t;return true});               // 2x forward
const mark=wheelMoves.length, newest=src;
run(491,640,()=>false);                                   // three samples lost
assert(posMs<=newest+2*35*0.5+4,'the deck ran on more than half a sample past the newest one: '+(posMs-newest));
run(641,1200,t=>{src=11300+2*490+0.4*(t-490);return true}); // then the source turns out to have slowed to 0.4x
assert(wheelMoves.slice(mark).every(v=>v>=0),'a late sample turned the deck back');
assert(Math.abs(posMs-(src-0.4*40))<30,'the deck did not rejoin the source: '+(posMs-src));
""")

    def test_paused_release_lands_on_the_source_even_where_it_rested_before(self):
        # Measured on two JP07: a paused scratch that came back to its start left the copy 16 ms short of it.
        self.js(RX + r"""
now+=25; scShadowApplyAll([E({scratch:false,positionMs:posMs,at:now})],false);         // let go
P('/Engine/Deck2/PlayState').state=false;
now+=100; scShadowApplyAll([E({playing:false,positionMs:posMs+50,at:now})],false);     // paused 50 ms on: one seek there
const rest=posMs, before=seeks;
assert.equal(scShadowAt[2].target,rest,'precondition: the deck was put on the paused source');
for(let i=1;i<=8;i++) { now+=25; scShadowApplyAll([E({playing:false,scratch:true,positionMs:rest-(i<5?i:8-i)*40,at:now})],false); scShadowScratchTimer.tick(); }
assert(Math.abs(posMs-rest)>10,'precondition: the frames are still a sample short of the source: '+(posMs-rest));
now+=45; scShadowApplyAll([E({playing:false,scratch:false,positionMs:rest,at:now})],false);
assert.equal(seeks,before+1,'the paused deck was left short of the source');
assert(Math.abs(posMs-rest)<1);
""")

    def test_release_far_from_the_source_still_seeks(self):
        self.js(RX + r"""
const beforeSeeks=seeks;
now+=25;
scShadowApplyAll([E({scratch:false,positionMs:posMs+400,at:now})],false);
assert.equal(seeks,beforeSeeks+1,'a release 400 ms off the source must land with one seek');
""")

    def test_stock_mirror_is_on_only_while_a_linked_deck_is_held(self):
        self.js(RX + r"""
assert.equal(P(MIRROR_ON).state,true,'the mirror was not switched on for a held linked deck');
assert.deepEqual(mirrorWrites,[true]);
// Engine switches it off when the player goes away and comes back: on again within a second, not on every update.
P(MIRROR_ON)._s=false;
for(let i=0;i<8;i++){now+=100;scShadowApplyAll([E({positionMs:11500+i*100,at:now})],false);}
assert.deepEqual(mirrorWrites,[true],'rewritten on every update');
now+=400; scShadowApplyAll([E({positionMs:12700,at:now})],false);
assert.deepEqual(mirrorWrites,[true,true]);
// The player ejects or goes: its last held deck is unloaded, the mirror goes off with it.
now+=100; scShadowApplyAll([],false);
now+=400; scShadowApplyAll([],false);
assert.equal(scShadowAt[2],undefined,'precondition: the deck was unloaded');
assert.deepEqual(mirrorWrites,[true,true,false]);
""")

    def test_stock_mirror_is_not_touched_without_engine_link(self):
        # A CDJ on Pro DJ Link has no Engine Link id: nothing to switch, our own updates carry the scratch.
        self.js(RX.replace("link:'{l}',", "link:'',") + r"""
assert.deepEqual(mirrorWrites,[]);
assert.equal(scShadowAt[2].scratchMirror,null);
assert(posMs<11500-100,'the deck did not follow our own update: '+posMs);
now+=25; scShadowApplyAll([],false); now+=400; scShadowApplyAll([],false);
assert.deepEqual(mirrorWrites,[]);
""")

    def test_fast_accelerating_spin_is_carried_by_the_jog(self):
        # The maintainer's "很快很快的加速转…一卡一卡". Measured on two JP07: a whip of the platter passes 20x
        # within 60 ms. Taking every mirror sample more than 500 ms on (14x) for a jump, the copy stood still
        # and went by seeks, 4 a second; the jog itself is exact to 50x.
        self.js(RX + r"""
const start=now, beforeSeeks=seeks;
const speed=t=>24*(t<60?t/60:t<160?1:t<220?(220-t)/60:0);   // 0 -> 24x in 60 ms, held 100 ms, down in 60 ms
let src=11300, stood=0, frames=0, most=0, last=posMs;
for(let t=1;t<=600;t++) {
    now=start+t; src+=speed(t);
    if(t%35===0) P(MIRROR).value=src*48;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:src,at:now})],false);
    if(t%16!==0) continue;
    const mark=wheelMoves.length;
    frameOnly=true; scShadowScratchTimer.tick(); frameOnly=false;
    most=Math.max(most,...wheelMoves.slice(mark).map(Math.abs));
    if(t>90&&t<230) { frames++; if(Math.abs(posMs-last)<1) stood++; }
    last=posMs;
}
assert(frames>=8,'insufficient coverage');
assert.equal(seeks,beforeSeeks,'a fast spin was followed by seeks');
assert.equal(stood,0,'the copy stood still on '+stood+' of '+frames+' frames of the spin');
assert(most>4000&&most<=16000,'jog ticks a frame outside the measured range: '+most);
assert(Math.abs(posMs-src)<40,'the copy did not come to rest on the source: '+(posMs-src));
""")

    # Room for a whip either way.
    FAR = RX.replace('posMs=10000', 'posMs=20000').replace('11500', '21500').replace('positionMs:10000', 'positionMs:20000')

    def test_whip_stopped_dead_comes_to_rest_without_running_on_or_coming_back(self):
        # The line ran half a sample past the newest one before the deck rested: 335 ms at 24x, and 272 ms back
        # again (683402d). It ends 60 ms on at most, what a scratch at 3.5x runs on; nothing comes back.
        for speed in (3.5, 24, 50):
            for stop in (280, 300, 315):
                for direction in (1, -1):
                    for plant in (False, True):
                        with self.subTest(speed=speed, stop=stop, direction=direction, plant=plant):
                            self.js(self.FAR + (self.PLANT if plant else "") + "const speed=%s, stop=%d, direction=%d, plant=%s;" % (speed, stop, direction, str(plant).lower()) + r"""
const start=now, beforeSeeks=seeks; let src=21300, peak=-Infinity, back=0, last;
for(let t=1;t<=1000;t++) {
    now=start+t; if(t<=stop) src+=direction*speed;
    if(plant) stepPlant(t);
    if(t%35===0) P(MIRROR).value=src*48;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:src,at:now})],false);
    if(t%16!==0) continue;
    if(plant) frame(); else scShadowScratchTimer.tick();
    const deck=plant?truePos:posMs;
    if(t>=stop) { peak=Math.max(peak,direction*(deck-src)); if(last!==undefined) back=Math.max(back,-direction*(deck-last)); last=deck; }
}
assert.equal(seeks,beforeSeeks);
assert(peak<=65,'the copy ran '+peak.toFixed(0)+' ms past the stop');
assert(back<=1,'the copy came back '+back.toFixed(0)+' ms in one frame after the stop');
assert(direction*((plant?truePos:posMs)-src)>=-5,'the copy stayed short of the source');
""")

    def test_fast_spin_through_a_stalled_gui_thread_is_still_motion(self):
        # A frame that comes 48 or 96 ms late finds the mirror two or three samples on: 2.8 s of track at 40x,
        # which read as a jump (one seek) while a sample was judged by its distance alone.
        for stall in (48, 96):
            for begin in range(200, 236, 5):
                with self.subTest(stall=stall, begin=begin):
                    self.js(self.FAR + "const stall=%d, begin=%d;" % (stall, begin) + r"""
const start=now, beforeSeeks=seeks; let src=21300, most=0;
for(let t=1;t<=800;t++) {
    now=start+t; src+=40;
    if(t%35===0) P(MIRROR).value=src*48;
    const blocked=t>begin&&t<begin+stall;
    if(t%25===0&&!blocked) scShadowApplyAll([E({scratch:true,positionMs:src,at:now})],false);
    if(t%16===0&&!blocked) { const mark=wheelMoves.length; scShadowScratchTimer.tick(); most=Math.max(most,...wheelMoves.slice(mark).map(Math.abs)); }
}
assert.equal(seeks,beforeSeeks,'a stalled GUI thread turned a 40x spin into a seek');
assert(most<=16000,'jog ticks a frame: '+most);
assert(Math.abs(posMs-(src-40*40))<800,'the copy is not on the spin: '+(posMs-src));
""")

    def test_change_of_source_never_reads_as_a_stroke_back(self):
        # Our own update tells where the source was 40-80 ms before the mirror does. Put into the mirror's line
        # when that went away, it read as travel back: a steady 3.5x scratch forward got a 37 ms jog back (150 ms
        # at 80 ms). Measured between two JP07: our update is ~40 ms behind the mirror.
        for missing in ("-1", "NaN", "'throw'"):
            for drop in (500, 525, 550, 575, 600):
                for delay in (40, 80):
                    for plant in (False, True):
                        with self.subTest(missing=missing, drop=drop, delay=delay, plant=plant):
                            self.js(RX + (self.PLANT if plant else "") + "const drop=%d, delay=%d, plant=%s, missing=%s;" % (drop, delay, str(plant).lower(), missing) + r"""
const start=now, beforeSeeks=seeks, read=P(MIRROR); let back=0, last;
for(let t=1;t<=1200;t++) {
    now=start+t; if(plant) stepPlant(t);
    if(t%35===0) read._v=(11300+3.5*t)*48;
    if(t>=drop&&t<drop+150) { if(missing==='throw') scShadowAt[2].scratchMirror={get value(){throw Error('gone')}}; else read._v=missing; }
    else scShadowAt[2].scratchMirror=read;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:11300+3.5*Math.max(0,t-delay),at:now})],false);
    if(t%16!==0) continue;
    const mark=wheelMoves.length;
    if(plant) frame(); else scShadowScratchTimer.tick();
    const deck=plant?truePos:posMs;
    if(t>=drop-50&&t<drop+400) { for(const ticks of wheelMoves.slice(mark)) back=Math.max(back,-ticks/20); if(last!==undefined) back=Math.max(back,last-deck); }
    last=deck;
}
assert(back<=0.1,'the change of source moved the copy back '+back.toFixed(1)+' ms');
assert.equal(seeks,beforeSeeks);
assert(Math.abs((plant?truePos:posMs)-(11300+3.5*(1200-50)))<70,'the copy is not on the source again: '+((plant?truePos:posMs)-(11300+3.5*1150)));
""")

    # frame() where a seek may come: its reads are allowed.
    TICK = "if(plant){const held=posMs;scShadowScratchTimer.tick();pending+=posMs-held;posMs=held}else scShadowScratchTimer.tick();"

    def test_source_at_rest_after_a_change_of_source_is_landed_on(self):
        # 5b1d645 dropped the line at a change of source and rested until the other source's next different sample
        # told a speed. A source that had stopped sent none: after a 50x whip the copy stayed 256 ms short of the
        # stop until the hand let go, and where our own update came 80 ms late it ran 373 ms past the stop and back.
        # Each source keeps its own line now; one at rest is a line like any other, and is landed on.
        for speed in (3.5, 24, 40, 50):
            for drop in (325, 330, 335, 340, 345, 350, 360):
                for delay in (40, 80):
                    for plant in (False, True):
                        with self.subTest(speed=speed, drop=drop, delay=delay, plant=plant):
                            self.js(self.FAR + (self.PLANT if plant else "") + "const speed=%s, drop=%d, delay=%d, plant=%s;" % (speed, drop, delay, str(plant).lower()) + r"""
const start=now, beforeSeeks=seeks; let src=21300, raw=src*48, peak=-Infinity, back=0, last;
for(let t=1;t<=1200;t++) {
    now=start+t; if(t<=280) src+=speed;
    if(plant) stepPlant(t);
    if(t%35===0) raw=src*48;
    P(MIRROR)._v=t>=drop&&t<drop+400?NaN:raw;      // the mirror goes as the source stops, and is back while it rests
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:21300+speed*Math.min(280,Math.max(0,t-delay)),at:now})],false);
    if(t%16!==0) continue;
    """ + self.TICK + r"""
    const deck=plant?truePos:posMs;
    if(t>=280) { peak=Math.max(peak,deck-src); if(last!==undefined) back=Math.max(back,last-deck); last=deck; }
}
const off=(plant?truePos:posMs)-src;
assert.equal(seeks,beforeSeeks);
assert(Math.abs(off)<=65,'the source stands still and the copy stayed '+off.toFixed(0)+' ms off it');
assert(peak<=65,'the copy ran '+peak.toFixed(0)+' ms past the stop');
// A whip comes to rest once. At 3.5x the two lines end up to 43 ms apart (each runs half its last sample on):
// the copy settles from the one to the other, 8 ms a frame at most.
assert(back<=(speed>10?1:10),'the copy came back '+back.toFixed(1)+' ms in one frame after the stop');
""")

    def test_sources_taking_turns_do_not_stop_the_scratch(self):
        # A mirror read that is a position on one frame and none on the next. Every change was a rest until the
        # next sample (5b1d645): taking turns each frame, a steady 3.5x scratch stood for 800 ms, then seeked.
        # The mirror leads again only once it has told a speed; until then our own update's line is still there.
        for frames in (1, 2, 4):
            with self.subTest(frames=frames):
                self.js(RX + "const frames=%d;" % frames + r"""
const start=now, beforeSeeks=seeks; let raw=11300*48, from=0, advance=0, idle=0, longest=0, back=0, last=posMs;
for(let t=1;t<=1400;t++) {
    now=start+t;
    if(t%35===0) raw=(11300+3.5*t)*48;
    P(MIRROR)._v=t>=400&&t<1200&&Math.floor((t-400)/(16*frames))%2===0?NaN:raw;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:11300+3.5*Math.max(0,t-40),at:now})],false);
    if(t===399) from=posMs;
    if(t===1199) advance=posMs-from;
    if(t%16!==0) continue;
    scShadowScratchTimer.tick();
    if(t>=400&&t<1200) { idle=Math.abs(posMs-last)<0.05?idle+1:0; longest=Math.max(longest,idle); back=Math.max(back,last-posMs); }
    last=posMs;
}
assert.equal(seeks,beforeSeeks,'sources taking turns ended in a seek');
assert(advance>=2500,'the copy went '+advance.toFixed(0)+' of the source\'s 2800 ms');
assert(longest*16<=48,'the copy stood for '+longest*16+' ms of a steady scratch');
assert(back<=0.1,'the copy went back '+back.toFixed(1)+' ms');
""")

    def test_change_of_source_in_a_whip_or_with_a_late_update_is_no_seek_and_no_step_back(self):
        # What the rules for a change of source are there for. Our own update's line is 40-80 ms of the source's
        # travel behind the copy when the mirror goes: the copy waits for it where it is, and joins it without
        # running past it. With the update itself late (a line at rest, far behind) that was a jog back, in a whip
        # a seek back; a mirror that led again from its first sample, with no speed yet, was a seek in a whip, and
        # one whose line was kept through 2.5 s without it came back as a seek to where the source had been then.
        for speed in (3.5, 24, 40):
            for drop in (500, 550, 575):
                for delay in (40, 80):
                    for late, away in ((0, 150), (90, 150)) + (((0, 2500),) if speed == 3.5 else ()):
                        for plant in (False, True):
                            with self.subTest(speed=speed, drop=drop, delay=delay, late=late, away=away, plant=plant):
                                self.js(RX + (self.PLANT if plant else "") + "const speed=%s, drop=%d, delay=%d, late=%d, away=%d, plant=%s;" % (speed, drop, delay, late, away, str(plant).lower()) + r"""
const start=now, beforeSeeks=seeks, read=P(MIRROR); let src=11300, back=0, last;
for(let t=1;t<=drop+away+500;t++) {
    now=start+t; src+=speed; if(plant) stepPlant(t);
    if(t%35===0) read._v=src*48;
    scShadowAt[2].scratchMirror=t>=drop&&t<drop+away?{value:-1}:read;
    if(t%25===0&&!(t>=drop-30&&t<drop-30+late)) scShadowApplyAll([E({scratch:true,positionMs:11300+speed*Math.max(0,t-delay),at:now})],false);
    if(t%16!==0) continue;
    const mark=wheelMoves.length;
    """ + self.TICK + r"""
    const deck=plant?truePos:posMs;
    if(t>=drop-50) { for(const ticks of wheelMoves.slice(mark)) back=Math.max(back,-ticks/20); if(last!==undefined) back=Math.max(back,last-deck); }
    last=deck;
}
assert(back<=0.1,'the change of source moved the copy back '+back.toFixed(1)+' ms');
// Our own update 90 ms late in a whip is a jump when it comes, as it was without the mirror: one seek on.
assert(seeks-beforeSeeks<=(speed>10&&late?1:0),'the change of source cost '+(seeks-beforeSeeks)+' seeks');
const off=(plant?truePos:posMs)-(src-speed*50);
assert(Math.abs(off)<20*speed,'the copy is not on the source again: '+off.toFixed(0));
""")

    def test_scratch_carried_on_by_our_own_update_lands_where_it_stops(self):
        # The wait at a change of source ends after 100 ms. Left on, the way the copy went when the mirror was lost
        # held it back for good: a stroke back and a stop on our own update left it 177 ms off the source (24x).
        for speed in (3.5, 24):
            for drop in (150, 200, 250, 300, 350):
                for delay in (40, 80):
                    for plant in (False, True):
                        with self.subTest(speed=speed, drop=drop, delay=delay, plant=plant):
                            self.js(self.FAR + (self.PLANT if plant else "") + "const speed=%s, drop=%d, delay=%d, plant=%s;" % (speed, drop, delay, str(plant).lower()) + r"""
const start=now, beforeSeeks=seeks;
const source=t=>21300+speed*(t<400?t:t<700?800-t:100);     // forward, back, and a stop
for(let t=1;t<=1400;t++) {
    now=start+t; if(plant) stepPlant(t);
    if(t%35===0) P(MIRROR)._v=t<drop?source(t)*48:-1;      // the mirror goes for good in the stroke forward
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:source(Math.max(0,t-delay)),at:now})],false);
    if(t%16===0) { """ + self.TICK + r""" }
}
const off=(plant?truePos:posMs)-source(1400);
assert.equal(seeks,beforeSeeks);
assert(Math.abs(off)<=65,'the copy rests '+off.toFixed(0)+' ms off the source');
""")

    def test_missed_jog_at_scratch_speed_is_made_up_as_soon_as_before_the_whip_allowance(self):
        # 5b1d645 gave the deck's report 100 ms at every speed, for a whip's sake (11 ms past the stop at 24x with
        # 80). At 3.5x that is 70 ms of track more a deck may lag before it is told: one that does 80 % of the ticks
        # was 473 ms behind at the stop instead of 401, and on the source 304 ms after it instead of 256.
        for gain, lag, landing in ((0.8, 402, 256), (0.985, 266, 96)):
            with self.subTest(gain=gain):
                self.js(RX + "const gain=%s, lag=%d, landing=%d;" % (gain, lag, landing) + r"""
let truePos=posMs, pending=0, atStop=0, settled=null; const trail=[], start=now;
for(let t=1;t<=8200;t++) {
    now=start+t;
    if(t%12===0) { truePos+=pending*gain; pending=0; }
    trail.push(truePos);
    if(t%35===0) posMs=trail[Math.max(0,trail.length-21)];
    const src=11300+3.5*Math.min(6000,t);                  // six seconds at 3.5x, then a stop
    if(t%35===0) P(MIRROR)._v=src*48;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:11300+3.5*Math.max(0,Math.min(6000,t-40)),at:now})],false);
    if(t%16!==0) continue;
    const held=posMs; scShadowScratchTimer.tick(); pending+=posMs-held; posMs=held;
    if(t===6000) atStop=src-truePos;
    if(t>6050&&settled===null&&Math.abs(truePos-src)<=65) settled=t-6000;
}
assert(atStop<=lag,'the deck was '+atStop.toFixed(0)+' ms behind at the stop');
assert(settled!==null&&settled<=landing,'the deck was on the source '+settled+' ms after the stop');
""")

    def test_no_frame_sends_more_than_the_jog_was_measured_at(self):
        # 16000 ticks a frame (800 ms, 50x) is as far as the jog was measured on a JP07 (19.8-20.3 ticks a ms).
        self.js(RX + r"""
const start=now, mark=wheelMoves.length;
const speed=t=>t<300?40:t<400?40*(400-t)/100:0;     // 40x, then down to a stop
let src=11300;
for(let t=1;t<=700;t++) {
    now=start+t; src+=speed(t);
    if(t%35===0) P(MIRROR).value=src*48;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:src,at:now})],false);
    if(t%16===0&&(t<150||t>190)) scShadowScratchTimer.tick();   // and the GUI thread stalls for 50 ms
}
assert.equal(Math.max(...wheelMoves.slice(mark).map(Math.abs)),16000,'the frames were not held to the measured range');
assert(Math.abs(posMs-src)<40,'what the cap held back was not made up: '+(posMs-src));
""")

    def test_report_of_before_a_seek_is_not_taken_for_a_slip(self):
        # Measured: for 20 ms after a seek TrackPosition still reads where the deck was. The trim took that
        # for a slip and pushed the deck 15 % of the seek past its target (+290 ms after 1.9 s), then crept back.
        self.js(RX + self.PLANT + r"""
const start=now, beforeSeeks=seeks; let high=-Infinity;
for(let t=1;t<=800;t++) {
    now=start+t; stepPlant(t);
    const src=t<200?11300:16300;                  // a cue 5 s on, under the hand
    if(t%35===0) P(MIRROR).value=src*48;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:src,at:now})],false);
    if(t%16===0) { const before=posMs; scShadowScratchTimer.tick(); pending+=posMs-before; posMs=before; }   // frame(), with a seek's reads allowed
    high=Math.max(high,truePos);
}
assert.equal(seeks,beforeSeeks+1,'a jump of 5 s is one seek');
assert(high<=16300+5,'the deck was pushed '+(high-16300).toFixed(0)+' ms past the seek');
assert(Math.abs(truePos-16300)<5,'the deck is not on the source: '+(truePos-16300));
""")

    def test_stock_mirror_goes_off_when_the_player_leaves_during_a_replacement_load(self):
        # The slot's state is new from the moment the source loads another track: who the mirror is on for must outlive it.
        self.js(RX + r"""
assert.equal(P(MIRROR_ON).state,true);
now+=25; scShadowApplyAll([E({id:'replacement',path:'/data/enginex/shadow-v3/peer-1/b.flac',scratch:true})],false);
assert.equal(P('/Engine/Deck2/ExternalScratchWheelTouch').state,false);
now+=25; scShadowApplyAll([],false);              // the player goes before the new copy is held
now+=400; scShadowApplyAll([],false);
assert.equal(scShadowAt[2],undefined,'precondition: the slot was given up');
assert.equal(P(MIRROR_ON).state,false,'the mirror of a player that left was left on');
""")

    def test_stock_mirror_is_shared_by_two_slots_and_follows_a_slot_to_another_player(self):
        self.js(RX + r"""
const four=o=>E(Object.assign({deck:4,id:'four',path:'/data/enginex/shadow-v3/peer-1/four.flac',scratch:false},o));
const OTHER='/Engine/BeatInfoSenderControl/{m}/Enabled';
now+=25; scShadowApplyAll([E({scratch:false}),four({})],false);
now+=1500; scShadowApplyAll([E({scratch:false}),four({})],false);
// One of the player's two decks goes: the other still needs the mirror.
now+=25; scShadowApplyAll([four({})],false); now+=400; scShadowApplyAll([four({})],false);
assert.equal(scShadowAt[2],undefined,'precondition: Deck 2 was unloaded');
assert.equal(P(MIRROR_ON).state,true,'unloading one slot switched off the mirror another slot uses');
// The slot gets another player's track: that player's mirror goes on, the first one's off.
now+=25; scShadowApplyAll([four({id:'other',path:'/data/enginex/shadow-v3/peer-2/x.flac',link:'{m}'})],false);
now+=1500; scShadowApplyAll([four({id:'other',path:'/data/enginex/shadow-v3/peer-2/x.flac',link:'{m}'})],false);
assert.equal(P(OTHER).state,true,'the new player\'s mirror was not switched on');
assert.equal(P(MIRROR_ON).state,false,'the mirror of the player the slot left stayed on');
now+=25; scShadowApplyAll([],false); now+=400; scShadowApplyAll([],false);
assert.equal(P(OTHER).state,false);
""")

    def test_unusable_mirror_sample_leaves_our_own_update_and_no_trace(self):
        # One NaN was taken for a sample: speed and position became NaN and the copy never moved again.
        for bad in ("NaN", "Infinity", "-48000", "undefined", "null"):
            with self.subTest(bad=bad):
                self.js(RX + "const bad=" + bad + r""";
const beforeSeeks=seeks;
P(MIRROR)._v=bad; now+=16; scShadowScratchTimer.tick();
const start=now, mark=posMs;
for(let t=1;t<=700;t++) {
    now=start+t;
    if(t%35===0) P(MIRROR)._v=t>300&&t<400?bad:(11300+2*t)*48;      // and three more in the middle of the stroke
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:11300+2*t,at:now})],false);
    if(t%16===0) scShadowScratchTimer.tick();
}
const at=scShadowAt[2];
assert([at.scratchX,at.scratchMir.vel,at.scratchMir.src,at.scratchOwn.vel,at.scratchOwn.src].every(Number.isFinite),'an unusable sample stayed in the follower');
assert(Math.abs(posMs-(11300+2*(700-50)))<60,'the copy is not on the source again: '+(posMs-12600));
assert.equal(seeks,beforeSeeks);
""")

    def test_failing_mirror_read_does_not_stop_the_frame(self):
        self.js(RX + r"""
scShadowAt[2].scratchMirror={get value(){throw Error('mirror unavailable')}};
const start=now, mark=posMs;
for(let t=1;t<=400;t++) {
    now=start+t;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:11300+2*t,at:now})],false);
    if(t%16===0) assert.doesNotThrow(()=>scShadowScratchTimer.tick(),'the mirror read stopped the frame');
}
assert(posMs>mark+500,'our own update did not carry the scratch: '+(posMs-mark));
""")

    def test_tiny_late_sample_cannot_reverse_a_forward_glide(self):
        self.js(RX + r"""
const start=now; let src=11300;
for(let t=1;t<=900;t++) {
    now=start+t;
    if(t%35===0) {
        if(t<=490) {src=11300+2*t;P(MIRROR).value=src*48;}
        else if(t>=641) {src=12280+0.02*(t-490);P(MIRROR).value=src*48;}
    }
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:src,at:now})],false);
    if(t%16===0) {
        const before=wheelMoves.length; scShadowScratchTimer.tick();
        if(t>=491) assert(wheelMoves.slice(before).every(v=>v>=0),'a late forward sample turned the jog back');
    }
}
""")

    def test_missed_jog_is_made_up_once_the_deck_is_slow(self):
        # A deck that does only 80 % of the ticks: the report tells once the motion stops.
        rx = RX.replace('posMs+=t/20', 'posMs+=t/20*0.8').replace('posMs-=t/20', 'posMs-=t/20*0.8')
        self.js(rx + r"""
const start=now;
for(let t=1;t<=3600;t++) {
    now=start+t;
    if(t%25===0) scShadowApplyAll([E({scratch:true,positionMs:t<=600?11300+0.8*t:11780,at:now})],false);
    if(t%16===0) scShadowScratchTimer.tick();
}
assert(Math.abs(posMs-11790)<5,'the lost jog was not made up at rest: '+posMs);
""")

    def test_paused_release_inside_35_ms_still_lands_after_an_earlier_landing_check(self):
        self.js(RX + r"""
now+=25; scShadowApplyAll([E({scratch:false,positionMs:posMs,at:now})],false);
P('/Engine/Deck2/PlayState').state=false;
now+=100; scShadowApplyAll([E({playing:false,positionMs:posMs+50,at:now})],false);
const rest=posMs;
posMs-=20;                                          // a landing error uses up the one check
now+=100; scShadowApplyAll([E({playing:false,positionMs:rest,at:now})],false);
assert.equal(scShadowAt[2].check,0,'precondition: the earlier landing check is done');
const before=seeks;
for(let i=1;i<=8;i++) { now+=25; scShadowApplyAll([E({playing:false,scratch:true,positionMs:rest-(i<5?i:8-i)*40,at:now})],false); scShadowScratchTimer.tick(); }
assert(Math.abs(posMs-rest)>10&&Math.abs(posMs-rest)<=35,'precondition: release drift '+(posMs-rest));
now+=45; scShadowApplyAll([E({playing:false,scratch:false,positionMs:rest,at:now})],false);
assert.equal(seeks,before+1,'the paused release inside 35 ms did not land on the source');
assert(Math.abs(posMs-rest)<1);
""")

    def test_sender_samples_at_once_when_the_platter_is_touched(self):
        # A resting deck is sampled 5 times a second: measured, the other player learnt of a scratch 130-270 ms late,
        # and a fast one was by then too far for the jog.
        sender = SENDER.replace("let pos=10000*48, playing=true,", "const woke={}; let pos=10000*48, playing=false,") \
                       .replace("globalThis.scTr=path=>({", "globalThis.scTr=path=>({stateChanged:{connect(f){(woke[path]=woke[path]||[]).push(f)}},")
        self.js(sender + r"""
for(let i=0;i<20;i++){now+=200;scShadowTick();}
assert.equal(scShadowTickTimer.interval,200,'precondition: a resting deck is sampled 5 times a second');
for(const name of ['ExternalScratchWheelTouch','PlatterUserAction']) {
    scShadowTickTimer.interval=200;
    assert(woke['/Engine/Deck1/'+name],'no touch signal is listened to: '+name);
    woke['/Engine/Deck1/'+name].forEach(f=>f());
    assert.equal(scShadowTickTimer.interval,40,'a hand on the platter did not wake the sampling: '+name);
}
""")

    def test_sender_reuses_sampling_buffers_at_40hz(self):
        self.js(SENDER + r"""
touch=true; now+=25; scShadowTick();
const buffers=[scShadowPrev[1],scShadowSampleSpare[1]], post=scShadowOwn[0], sent=scShadowSent[0];
for(let i=0;i<80;i++) {now+=25;pos-=15*48;scShadowTick();assert(buffers.includes(scShadowPrev[1]));}
assert.strictEqual(scShadowOwn[0],post); assert.strictEqual(scShadowSent[0],sent);
""")

    def test_display_reload_fetches_the_new_session_metadata(self):
        self.js(r"""
let metaRequests=0;
scShadowDisplay[2]={key:'old-session',meta:{detailPending:false,cues:[{ms:100}],beats:[{ms:250}]}};
scShadowGridCache[2]={stale:true};
globalThis.XMLHttpRequest=class {
    open(){}
    send(){metaRequests++;this.readyState=4;this.status=200;this.responseText=JSON.stringify({key:'new-session',detailPending:false,cues:[{ms:600}],beats:[{ms:300}]});this.onreadystatechange();}
};
scShadowApplyDisplay([{deck:2,key:'new-session',displayOnly:true}]);
assert.equal(metaRequests,1,'reload kept the completed metadata of the previous session');
assert.equal(scShadowDisplay[2].meta.cues[0].ms,600);
assert.equal(scShadowDisplay[2].meta.beats[0].ms,300);

""")


if __name__ == "__main__":
    unittest.main()
