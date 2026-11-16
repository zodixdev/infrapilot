#!/usr/bin/env bash
set -euo pipefail

fail() {
    printf '%s\n' "$*" >&2
    exit 1
}

if (( $# != 2 && $# != 4 )); then
    fail 'Usage: provision-topics.sh {local|production} BOOTSTRAP_SERVERS [--command-config FILE]'
fi

case "$1" in
    local) replication_factor=1; min_isr=1 ;;
    production) replication_factor=3; min_isr=2 ;;
    *) fail 'Select the local or production Kafka profile.' ;;
esac

bootstrap_servers=$2
[[ -n "$bootstrap_servers" ]] || fail 'Kafka bootstrap servers are required.'
kafka_args=(--bootstrap-server "$bootstrap_servers")
if (( $# == 4 )); then
    [[ "$3" == --command-config && -f "$4" && -r "$4" ]] ||
        fail 'Supply a readable AdminClient properties file with --command-config.'
    kafka_args+=(--command-config "$4")
fi

manifest="$(dirname -- "${BASH_SOURCE[0]}")/topics.conf"
[[ -r "$manifest" ]] || fail 'Kafka topic manifest is unreadable.'
[[ -x /opt/kafka/bin/kafka-topics.sh ]] || fail 'Run inside the pinned Apache Kafka image.'
command -v timeout >/dev/null || fail 'The provisioning container requires timeout.'

run_kafka() {
    # The CLI may inherit the broker heap within the Compose container.
    # Keep CLI diagnostics out of logs because an authentication error can carry private details.
    KAFKA_HEAP_OPTS='-Xms32m -Xmx192m' timeout -k 5 45 \
        /opt/kafka/bin/kafka-topics.sh "${kafka_args[@]}" "$@" </dev/null 2>/dev/null
}

# Validate the complete manifest before sending any command to Kafka. Build an exact
# regular expression for --topic, which Kafka treats as a pattern during describe.
selector=$(awk -F '|' '
    /^#/ || NF == 0 { next }
    NF != 4 || $1 !~ /^[a-z0-9][a-z0-9.-]*$/ || seen[$1]++ ||
        $2 !~ /^[1-9][0-9]*$/ || $2 > 2147483647 ||
        !($3 == "compact" && $4 == "-" || $3 == "delete" && $4 ~ /^[1-9][0-9]*$/) {
        invalid = 1
        exit 1
    }
    {
        name = $1
        gsub(/[.]/, "[.]", name)
        patterns = patterns (patterns == "" ? "" : "|") name
    }
    END {
        if (invalid || patterns == "") exit 1
        print "^(" patterns ")$"
    }
' "$manifest") || fail 'Kafka topic manifest has invalid or duplicate entries.'

inspect_topics() {
    local description
    description=$(run_kafka --describe --if-exists --topic "$selector") || {
        printf 'Kafka topic describe failed. Check broker health, connectivity and admin permissions.\n' >&2
        return 1
    }

    # Kafka 4.3.2 describe separates metadata fields with tabs. Within Configs, a
    # comma before a setting name starts the next entry. Other commas belong to
    # the value, such as the second policy in cleanup.policy=compact,delete.
    awk -F $'\t' -v rf="$replication_factor" -v min_isr="$min_isr" '
        function check_config(topic, configs, name, expected,    value, key, found, boundary) {
            while (configs != "") {
                boundary = match(configs, /,[a-zA-Z0-9._-]+=/)
                value = boundary ? substr(configs, 1, boundary - 1) : configs
                configs = boundary ? substr(configs, boundary + 1) : ""
                key = index(value, "=")
                if (key && substr(value, 1, key - 1) == name && (found++ || substr(value, key + 1) != expected)) {
                    drift(topic, name, expected)
                    return
                }
            }
            if (!found) drift(topic, name, expected)
        }

        function drift(topic, setting, expected) {
            printf "Kafka topic %s has drift in %s (expected %s).\n", topic, setting, expected > "/dev/stderr"
            invalid = 1
        }

        function field_value(prefix,    field_index) {
            for (field_index = 1; field_index <= NF; field_index++) {
                if (index($field_index, prefix) == 1) return substr($field_index, length(prefix) + 1)
            }
            return ""
        }

        NR == FNR {
            if ($0 ~ /^#/ || $0 == "") next
            split($0, values, "[|]")
            topics[++topic_count] = values[1]
            partitions[values[1]] = values[2] + 0
            cleanup[values[1]] = values[3]
            retention[values[1]] = values[4]
            next
        }

        {
            topic = field_value("Topic: ")
            if (!(topic in partitions)) next

            partition_count = field_value("PartitionCount: ")
            if (partition_count != "") {
                if (partition_count !~ /^[0-9]+$/ || partition_count + 0 != partitions[topic]) {
                    drift(topic, "partition count", partitions[topic])
                }
                described[topic]++
                if (field_value("ReplicationFactor: ") != rf) drift(topic, "replication factor", rf)
                configs = field_value("Configs: ")
                check_config(topic, configs, "cleanup.policy", cleanup[topic])
                check_config(topic, configs, "min.insync.replicas", min_isr)
                if (retention[topic] != "-") check_config(topic, configs, "retention.ms", retention[topic])
                next
            }

            partition = field_value("Partition: ")
            if (partition !~ /^[0-9]+$/ || partition + 0 >= partitions[topic] || seen[topic, partition + 0]++) {
                invalid_metadata[topic] = 1
                next
            }
            observed[topic]++
            replicas = field_value("Replicas: ")
            if (replicas !~ /^[0-9]+(,[0-9]+)*$/ || split(replicas, replica_ids, ",") != rf) {
                drift(topic, "replica count on partition " partition, rf)
            }
        }

        END {
            for (i = 1; i <= topic_count; i++) {
                topic = topics[i]
                if (!invalid_metadata[topic] && !described[topic] && !observed[topic]) {
                    print topic
                } else if (invalid_metadata[topic] || described[topic] != 1 || observed[topic] != partitions[topic]) {
                    drift(topic, "partition metadata", partitions[topic] " partitions")
                }
            }
            if (invalid) exit 1
        }
    ' "$manifest" - <<< "$description"
}

missing=$(inspect_topics) || exit 1
if [[ -n "$missing" ]]; then
    while IFS='|' read -r topic partitions cleanup retention || [[ -n "$topic" ]]; do
        if [[ -z "$topic" || "$topic" == \#* ]]; then
            continue
        fi
        if [[ $'\n'"$missing"$'\n' != *$'\n'"$topic"$'\n'* ]]; then
            continue
        fi

        configs=(--config "cleanup.policy=$cleanup" --config "min.insync.replicas=$min_isr")
        if [[ "$retention" != '-' ]]; then
            configs+=(--config "retention.ms=$retention")
        fi
        run_kafka --create --if-not-exists --topic "$topic" --partitions "$partitions" \
            --replication-factor "$replication_factor" "${configs[@]}" >/dev/null ||
            fail "Kafka topic creation failed for $topic. Check broker health, capacity and permissions before retrying."
    done < "$manifest"

    # Allow for metadata propagation after the controller accepts a create request.
    for delay in 0 1 2 4; do
        sleep "$delay"
        missing=$(inspect_topics) || exit 1
        [[ -n "$missing" ]] || break
    done
fi

[[ -z "$missing" ]] || fail "Kafka topics are still missing after creation. Rerun after checking broker health: $missing"
printf 'Verified Kafka topic definitions (RF=%s, min ISR=%s).\n' "$replication_factor" "$min_isr"
