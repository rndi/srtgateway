# Example SRT gateway container

## Usage

* Build

```
docker build -t srtgateway .
```

* Debug run

```
docker run --net=host -it --rm --name srtgateway \
-e CONTAINER_CONFIG="{\"configurationBlobLocation\":\"https://storagesample.blob.core.windows.net;SharedAccessSignature=sv=2015-04-05&amp;sr=b&amp;si=sample-policy-635959936145100803&amp;sig=9aCzs76n0E7y5BpEi2GvsSv433BZa22leDOZXX%2BXXIU%3D\",\"eventHubConnection\":{\"namespace\":\"qa-srt-hub-event-hub\",\"name\":\"test\",\"sharedAccessPolicy\":\"send\",\"sharedAccessKey\":\"ctzMq410TV3wS7upTBcunJTDLEJwMAZuFPfr0mrrA08=\"},\"telemetryFrequency\":5}" \
-e APP_CONFIG="{\"channel_name\":\"Demo Channel\",\"channel_id\":\"69414c3-f9c8-41a3-9f3b-151ca71eb445\",\"app_data\":{\"user_account\":\"max.mustermann@goodcompany.com\"},\"inputs\":[{\"id\":\"INPUT\",\"name\":\"Source Stream\",\"url\":\"srt:\/\/0.0.0.0:4900\",\"protocol\":\"srt\",\"parameters\":[{\"name\":\"srt_mode\",\"value\":\"listener\"},{\"name\":\"srt_passphrase\",\"value\":\"#1234567890\"},{\"name\":\"srt_latency\",\"value\":\"200\"}]}],\"outputs\":[{\"id\":\"OUTPUT_1\",\"name\":\"Primary\",\"url\":\"udp:\/\/239.42.42.42:4243\",\"protocol\":\"udp\",\"parameters\":[]},{\"id\":\"OUTPUT_2\",\"name\":\"Secondary\",\"url\":\"srt:\/\/0.0.0.0:1234\",\"protocol\":\"srt\",\"parameters\":[{\"name\":\"srt_mode\",\"value\":\"listener\"},{\"name\":\"srt_passphrase\",\"value\":\"ThisIsMyPass\"},{\"name\":\"srt_latency\",\"value\":\"200\"}]}]}" \
srtgateway
```
