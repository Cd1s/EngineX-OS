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
            maxError=Math.max(maxError,Math.abs(posMs-expected));
        }
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
