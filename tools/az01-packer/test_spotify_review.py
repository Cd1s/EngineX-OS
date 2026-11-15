"""PR #294: source filtering uses the stock model's source kind, not its label."""
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import unittest

REPO = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("spotify_review_packs", REPO / "tools/ui-mod/build_packs.py")
bp = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bp)


def function(source, name):
    start = source.index("function " + name + "(")
    end = source.index("{", start) + 1
    depth = 1
    while depth:
        depth += (source[end] == "{") - (source[end] == "}")
        end += 1
    return source[start:end]



class SpotifyReviewTest(unittest.TestCase):
    def test_same_named_storage_rows_remain_visible_and_enter_normally(self):
        fixtures = json.loads(os.environ.get("SPOTIFY_REVIEW_QRC_ROOTS", '["/root/build/sc5000-lab/5.3.0/qrc"]'))
        for fixture in fixtures:
            with self.subTest(fixture=fixture):
                qrc = Path(fixture)
                stock = qrc / "Skins/Embedded/Views/BrowserSource/ConnectedSource.qml"
                self.assertTrue(stock.is_file(), str(stock))
                bp.ROOT = "Skins/Embedded/"
                patches = bp.edits(False)
                rel = "Skins/Embedded/Views/BrowserSource/ConnectedSource.qml"
                patched = stock.read_text()
                for old, new in patches[rel]:
                    patched = bp.patch(patched, old, new, rel)
                height = re.search(r"height: (.*)\n\s*visible:", patched).group(1)
                visible = re.search(r"visible: (.*)\n\s*Component.onCompleted:", patched).group(1)
                available = re.search(r"visible: (.*)", bp.AVAILABLE_SOURCE).group(1)
                kind = function(bp.SERVICES_MAIN, "scSourceTypeText") if "function scSourceTypeText(" in bp.SERVICES_MAIN else ""
                hidden = function(bp.SOURCE_FOOTER, "scHiddenSourceName")
                entries = function(bp.SOURCE_FOOTER, "scEntries")
                enter = bp.SOURCE_ENTER
                script = r'''
const assert=require('node:assert/strict');
const names=['TIDAL','Tidal','SoundCloud','Beatport LINK','Apple Music'];
let rows=[],opened=[],entered=0;
const model={roles:()=>({name:257,typeText:273}),rowCount:()=>rows.length,index:r=>r,
 data:(r,role)=>role===257?rows[r].name:rows[r].typeText};
const nav={availableSourceIndex:-1,connectedSourceIndex:0,enter:()=>entered++};
const Planck={librarianController:{browserSourceControllerPtr:()=>({connectedSourceModel:()=>model,availableSourceModel:()=>model})}};
const controller={navigation:()=>nav};
const root={scSourceNameRole:257,scServices:[],scSources:[],scScope:'spotify',scOpen:id=>opened.push(id)};
const connectedSource={expanded:true};
KIND
if (typeof scSourceTypeText==='function') root.scSourceTypeText=scSourceTypeText;
HIDDEN
ENTRIES
ENTER
connectedSource.scHiddenSourceName=scHiddenSourceName;
for(const name of names) {
 for(const typeText of ['Storage','Remote Library','Cloud','Streaming']) {
  const row={name,typeText}; rows=[row];
  assert.equal(scHiddenSourceName(name,typeText),typeText==='Streaming',name+' / '+typeText);
  const model=row;
  assert.equal(HEIGHT,typeText==='Streaming'?0:108,name+' height / '+typeText);
  assert.equal(VISIBLE,typeText!=='Streaming',name+' visible / '+typeText);
  assert.equal(AVAILABLE,typeText!=='Streaming',name+' available / '+typeText);
  const list=scEntries();
  assert.equal(list.length,typeText==='Streaming'?0:2,name+' keyboard list / '+typeText);
  opened=[];entered=0;scSourceEnter();
  assert.equal(entered,typeText==='Streaming'?0:1,name+' enter / '+typeText);
  if(name==='Apple Music' && typeText==='Streaming') assert.deepEqual(opened,['applemusic']);
 }
}
'''.replace("KIND", kind).replace("HIDDEN", hidden).replace("ENTRIES", entries).replace("ENTER", enter).replace("HEIGHT", height).replace("VISIBLE", visible).replace("AVAILABLE", available)
                result = subprocess.run(["node", "-e", script], capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_pr294_notes_keep_content_without_new_tone(self):
        content = (REPO / "docs/site/content.js").read_text()
        for marker in ("新增：Apple Music", "Web console → Music → **Spotify audio source**"):
            lines = [line for line in content.splitlines() if marker in line]
            self.assertEqual(len(lines), 1)
            self.assertNotIn('tone: "new"', lines[0])


if __name__ == "__main__":
    unittest.main()
