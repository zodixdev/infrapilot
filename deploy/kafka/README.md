# M03 Kafka topics

`topics.conf` declares the partition count and cleanup policy for the four M03 topics and their dead-letter queues. Each dead-letter queue uses one partition and seven days of retention. The resource state topic uses compaction with seven days of tombstone retention for projection recovery (ADR-0031). The delete-policy topics specify their age limit in milliseconds.

From the repository root, start the Compose broker and run the provisioner in its pinned Kafka container.

```bash
compose=(docker compose --project-name infrapilot --env-file .env -f deploy/compose/docker-compose.yml --profile stream)
"${compose[@]}" up -d --wait kafka
"${compose[@]}" exec -T kafka mkdir -p /tmp/infrapilot-kafka </dev/null
"${compose[@]}" cp deploy/kafka/topics.conf kafka:/tmp/infrapilot-kafka/topics.conf
"${compose[@]}" cp deploy/kafka/provision-topics.sh kafka:/tmp/infrapilot-kafka/provision-topics.sh
"${compose[@]}" exec -T kafka bash /tmp/infrapilot-kafka/provision-topics.sh local kafka:19092 </dev/null
```

Run the script in the same pinned image for other environments, with the script and manifest available in the same directory. Use `production` to create three replicas and require two in-sync replicas. Provide TLS or SASL settings through an AdminClient properties file mounted from a secret.

```bash
bash /provisioning/provision-topics.sh production kafka-bootstrap:9093 \
    --command-config /run/secrets/kafka-admin.properties
```

Grant the provisioning principal permission to create and describe the managed topics and read their configuration. The Kafka CLI also queries the cluster for partition reassignments during describe.

A run checks the existing topics before it attempts any creation. It fails on drift in partition counts, replica counts or configured topic settings. Resolve drift through the cluster's change procedure and rerun. Interrupted runs can be retried because the script creates only missing topics and checks their definitions after creation.
