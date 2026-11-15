`spotify-native-44100.ogg` is the original generated stereo C-major / 120 BPM
fixture used by the Spotify decryption tests. No commercial recording is used.
`spotify-matched-44100.mp3` encodes the same audio for cross-service cache,
analysis and Engine library import coverage:

```sh
ffmpeg -i spotify-native-44100.ogg -c:a libmp3lame -b:a 320k -map_metadata -1 spotify-matched-44100.mp3
```

Tests use these checked-in fixtures and do not require ffmpeg or live accounts.
