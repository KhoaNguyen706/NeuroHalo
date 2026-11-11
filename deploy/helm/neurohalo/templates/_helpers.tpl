{{/* The Postgres address every service uses. */}}
{{- define "neurohalo.databaseUrl" -}}
postgres://{{ .Values.postgres.user }}:{{ .Values.postgres.password }}@postgres:5432/{{ .Values.postgres.database }}
{{- end }}

{{/*
An init container that waits until the last Kafka topic exists. Topics are
created by the kafka-init Job, and automatic topic creation is off, so a
service that started earlier would fail on its first message.
*/}}
{{- define "neurohalo.waitForTopics" -}}
- name: wait-for-topics
  image: apache/kafka:4.0.0
  command:
    - /bin/sh
    - -c
    - |
      until /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:9092 --describe --topic eeg.events > /dev/null 2>&1; do
        echo "waiting for Kafka topics"; sleep 3
      done
{{- end }}
