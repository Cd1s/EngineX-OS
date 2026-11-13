"""Regression checks for the shadow scratch review findings (#240, #217)."""
import unittest
import shutil
import test_ui_mod

RX = r"""
const props=new Map();
let posMs=10000, seeks=0, wheelMoves=[];
globalThis.scShadowBent=null;
globalThis.scShadowBendTimer={interval:0,running:false,stop(){this.running=false},restart(){this.running=true}};
const P=path=>{
    let p=props.get(path);
    if(p)return p;
    p={_s:path.endsWith('/SongLoaded')||path.endsWith('/PlayState'),_v:path.endsWith('/SampleRate')?48000:0,
       string:path.endsWith('/SpeedRange')?'8':path.endsWith('/SyncMode')?'Off':'',
       entries:['4','8','10','20','50','100'],index:1,
       get state(){return this._s},set state(v){this._s=v},
       get value(){return this._v},set value(v){this._v=v;if(path.endsWith('/SetSongPosition')){posMs=v*400000;seeks++}},
       get unnormalized(){return path.endsWith('/TrackLength')?48000*400:path.endsWith('/TrackPosition')?posMs*48:0},
       incValue(t){wheelMoves.push(t);posMs+=t/23.5},
       decValue(t){wheelMoves.push(-t);posMs-=t/23.5}};
    props.set(path,p);
    return p;
};
globalThis.scTr=P;
let seq=0;
const E=(o)=>Object.assign({deck:2,path:'/data/enginex/shadow-v3/peer-1/a.flac',id:'1',playing:true,link:'{l}',positionMs:11500,at:now,pitch:0,bpm:128,currentBpm:128,jumps:0,seq:++seq},o);
scShadowApplyAll([E({positionMs:10000})],false);
now+=1500; posMs=11500;
scShadowApplyAll([E({})],false);
for(let i=1;i<=5;i++){now+=25;scShadowApplyAll([E({scratch:true,positionMs:11500-i*40,at:now})],false);}
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
}
assert(seeks-before<=4,'fast scratch flushed decoder '+(seeks-before)+' times per second');
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
