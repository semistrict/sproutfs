#!/usr/bin/env bash
# Dependent single reads on six disposable EC2 hosts in one availability zone
# of us-east-1: from a regional S3 Standard bucket, from a hot tier in an S3
# Express One Zone directory bucket in the hosts' zone, and from the cluster
# cache with each host's disk on an EBS gp3 volume, and the hot tier's fills
# on a cold run (docs/measurements/aws-hot-tier-2026-10-04.md). It is the AWS
# form of scripts/bench-hot-tier-gce.sh.
#
# Every host runs `sproutfs-restorebench node -cloud aws`: the real checkpoint
# store, page cache, peer server and table of peers, its cache on the raw
# block device of its gp3 volume, as a shard keeps its disk. The second host
# runs `sproutfs-restorebench walk`, which publishes from the first and then
# reads one page at a time on the second. Then every node stops and fio
# measures each gp3 volume raw (scripts/lib/shard-fio.sh).
#
# The run makes, and tags sproutfs-bench=<run id>: a VPC with one subnet in
# the zone SPROUTFS_AWS_ZONE_ID names (use1-az4 by default), its internet
# gateway, route table and gateway endpoints for S3 and S3 Express, a security
# group that admits SSH from this machine's address and every TCP port between
# the hosts, a key pair, an IAM role and instance profile that may reach the
# two buckets alone, a general purpose bucket and a directory bucket, and the
# hosts, each with a root volume and a gp3 volume for its cache.
# SPROUTFS_AWS_INSTANCE_TYPE is the hosts' type (m7i.xlarge by default), and
# SPROUTFS_AWS_GP3_GB, SPROUTFS_AWS_GP3_IOPS and SPROUTFS_AWS_GP3_MIBS the
# cache volumes' size, IOPS and MiB/s (64, 16000 and 500 by default).
# SPROUTFS_WALK_HOSTS, SPROUTFS_WALK_CODE, SPROUTFS_WALK_READS,
# SPROUTFS_WALK_PAGES, SPROUTFS_WALK_PAGES_4K and SPROUTFS_WALK_ROUNDS are as
# for the GCE script. The ambient AWS credentials are used throughout.
#
# `all` always deletes everything the run made and checks that nothing of it
# remains. `create`, `run` and `delete` expose the same steps; each takes the
# run id as its second argument. Each host also powers off, and so terminates
# and deletes its volumes, three hours after it boots. The SSH key and the
# built binary are kept in SPROUTFS_AWS_STATE (a directory under TMPDIR named
# by the run by default), and the results in the third argument.
set -euo pipefail
export AWS_PAGER=""
for name in SPROUTFS_WALK_HOSTS SPROUTFS_WALK_READS SPROUTFS_WALK_PAGES SPROUTFS_WALK_PAGES_4K SPROUTFS_WALK_ROUNDS \
    SPROUTFS_AWS_GP3_GB SPROUTFS_AWS_GP3_IOPS SPROUTFS_AWS_GP3_MIBS; do
    [[ ${!name:-} =~ ^[0-9]*$ ]] || { echo "$name is a number" >&2; exit 2; }
done
count=${SPROUTFS_WALK_HOSTS:-6}
code=${SPROUTFS_WALK_CODE:-4+2}
reads=${SPROUTFS_WALK_READS:-500}
pages=${SPROUTFS_WALK_PAGES:-1024}
pages4k=${SPROUTFS_WALK_PAGES_4K:-131072}
rounds=${SPROUTFS_WALK_ROUNDS:-3}
((count >= 2)) || { echo "SPROUTFS_WALK_HOSTS is at least 2" >&2; exit 2; }
[[ $code =~ ^[0-9]+\+[0-9]+$ ]] || { echo "SPROUTFS_WALK_CODE is k+m" >&2; exit 2; }
instance_type=${SPROUTFS_AWS_INSTANCE_TYPE:-m7i.xlarge}
[[ $instance_type =~ ^[a-z0-9]+\.[a-z0-9]+$ ]] || { echo "SPROUTFS_AWS_INSTANCE_TYPE is an EC2 type" >&2; exit 2; }
zone_id=${SPROUTFS_AWS_ZONE_ID:-use1-az4}
[[ $zone_id =~ ^use1-az[0-9]+$ ]] || { echo "SPROUTFS_AWS_ZONE_ID is a us-east-1 zone ID such as use1-az4" >&2; exit 2; }
gp3_gb=${SPROUTFS_AWS_GP3_GB:-64}
gp3_iops=${SPROUTFS_AWS_GP3_IOPS:-16000}
gp3_mibs=${SPROUTFS_AWS_GP3_MIBS:-500}

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
region=us-east-1
action=${1:-all}
run=${2:-sproutfs-aws-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/aws-hot-tier-$(date -u +%Y-%m-%d)}
[[ $run =~ ^sproutfs-aws-[0-9]{8}-[0-9]{6}$ ]] || { echo 'A run id is sproutfs-aws-YYYYmmdd-HHMMSS.' >&2; exit 2; }
state=${SPROUTFS_AWS_STATE:-${TMPDIR:-/tmp}/$run}
cloud=(aws --region "$region" --output text)
tag_key=sproutfs-bench
tags="Tags=[{Key=$tag_key,Value=$run},{Key=Name,Value=$run}]"
owned=(--filters "Name=tag:$tag_key,Values=$run")
regional_bucket=$run
express_bucket=$run--$zone_id--x-s3
objects=$run/objects
ssh_options=(-i "$state/key" -o "UserKnownHostsFile=$state/known_hosts" -o StrictHostKeyChecking=accept-new
    -o ConnectTimeout=10 -o BatchMode=yes)

create() {
    local account address vpc subnet gateway table group image attempt
    mkdir -p "$state"
    account=$("${cloud[@]}" sts get-caller-identity --query Account)
    address=$(curl -fsS https://checkip.amazonaws.com)
    [[ $address =~ ^[0-9.]+$ ]] || { echo "This machine's address is $address, not IPv4." >&2; return 1; }
    [[ -e "$state/key" ]] || ssh-keygen -q -t ed25519 -N '' -C "$run" -f "$state/key"
    "${cloud[@]}" ec2 import-key-pair --key-name "$run" --public-key-material "fileb://$state/key.pub" \
        --tag-specifications "ResourceType=key-pair,$tags" > /dev/null

    vpc=$("${cloud[@]}" ec2 create-vpc --cidr-block 10.99.0.0/16 \
        --tag-specifications "ResourceType=vpc,$tags" --query Vpc.VpcId)
    "${cloud[@]}" ec2 wait vpc-available --vpc-ids "$vpc"
    subnet=$("${cloud[@]}" ec2 create-subnet --vpc-id "$vpc" --cidr-block 10.99.1.0/24 \
        --availability-zone-id "$zone_id" --tag-specifications "ResourceType=subnet,$tags" --query Subnet.SubnetId)
    "${cloud[@]}" ec2 modify-subnet-attribute --subnet-id "$subnet" --map-public-ip-on-launch
    gateway=$("${cloud[@]}" ec2 create-internet-gateway \
        --tag-specifications "ResourceType=internet-gateway,$tags" --query InternetGateway.InternetGatewayId)
    "${cloud[@]}" ec2 attach-internet-gateway --internet-gateway-id "$gateway" --vpc-id "$vpc"
    table=$("${cloud[@]}" ec2 create-route-table --vpc-id "$vpc" \
        --tag-specifications "ResourceType=route-table,$tags" --query RouteTable.RouteTableId)
    "${cloud[@]}" ec2 create-route --route-table-id "$table" --destination-cidr-block 0.0.0.0/0 \
        --gateway-id "$gateway" > /dev/null
    "${cloud[@]}" ec2 associate-route-table --route-table-id "$table" --subnet-id "$subnet" > /dev/null
    # Both buckets are reached through gateway endpoints, as a deployment in
    # a VPC would reach them, and neither through the internet gateway.
    for service in s3 s3express; do
        "${cloud[@]}" ec2 create-vpc-endpoint --vpc-id "$vpc" --vpc-endpoint-type Gateway \
            --service-name "com.amazonaws.$region.$service" --route-table-ids "$table" \
            --tag-specifications "ResourceType=vpc-endpoint,$tags" > /dev/null
    done
    group=$("${cloud[@]}" ec2 create-security-group --group-name "$run" --vpc-id "$vpc" \
        --description "sproutfs benchmark $run" --tag-specifications "ResourceType=security-group,$tags" \
        --query GroupId)
    "${cloud[@]}" ec2 authorize-security-group-ingress --group-id "$group" \
        --ip-permissions "IpProtocol=tcp,FromPort=22,ToPort=22,IpRanges=[{CidrIp=$address/32}]" \
        "IpProtocol=tcp,FromPort=0,ToPort=65535,UserIdGroupPairs=[{GroupId=$group}]" > /dev/null

    "${cloud[@]}" s3api create-bucket --bucket "$regional_bucket" > /dev/null
    "${cloud[@]}" s3api put-bucket-tagging --bucket "$regional_bucket" \
        --tagging "TagSet=[{Key=$tag_key,Value=$run}]"
    "${cloud[@]}" s3api create-bucket --bucket "$express_bucket" --create-bucket-configuration \
        "Location={Type=AvailabilityZone,Name=$zone_id},Bucket={DataRedundancy=SingleAvailabilityZone,Type=Directory}" \
        > /dev/null
    "${cloud[@]}" s3control tag-resource --account-id "$account" \
        --resource-arn "arn:aws:s3express:$region:$account:bucket/$express_bucket" --tags "Key=$tag_key,Value=$run"

    "${cloud[@]}" iam create-role --role-name "$run" --tags "Key=$tag_key,Value=$run" \
        --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
            "Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}' > /dev/null
    "${cloud[@]}" iam put-role-policy --role-name "$run" --policy-name buckets --policy-document "{
        \"Version\": \"2012-10-17\", \"Statement\": [
        {\"Effect\": \"Allow\", \"Action\": \"s3:*\",
         \"Resource\": [\"arn:aws:s3:::$regional_bucket\", \"arn:aws:s3:::$regional_bucket/*\"]},
        {\"Effect\": \"Allow\", \"Action\": \"s3express:CreateSession\",
         \"Resource\": \"arn:aws:s3express:$region:$account:bucket/$express_bucket\"}]}"
    "${cloud[@]}" iam create-instance-profile --instance-profile-name "$run" \
        --tags "Key=$tag_key,Value=$run" > /dev/null
    "${cloud[@]}" iam add-role-to-instance-profile --instance-profile-name "$run" --role-name "$run"
    "${cloud[@]}" iam wait instance-profile-exists --instance-profile-name "$run"

    image=$("${cloud[@]}" ssm get-parameter --query Parameter.Value \
        --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64)
    # A new instance profile takes a few seconds to reach EC2.
    for attempt in {1..12}; do
        if "${cloud[@]}" ec2 run-instances --count "$count" --image-id "$image" \
            --instance-type "$instance_type" --key-name "$run" --subnet-id "$subnet" \
            --security-group-ids "$group" --iam-instance-profile "Name=$run" \
            --metadata-options HttpTokens=required --instance-initiated-shutdown-behavior terminate \
            --user-data "file://$repo/scripts/lib/aws-bench-startup.sh" \
            --block-device-mappings "[
                {\"DeviceName\": \"/dev/xvda\", \"Ebs\": {\"VolumeSize\": 16, \"VolumeType\": \"gp3\",
                 \"DeleteOnTermination\": true}},
                {\"DeviceName\": \"/dev/sdf\", \"Ebs\": {\"VolumeSize\": $gp3_gb, \"VolumeType\": \"gp3\",
                 \"Iops\": $gp3_iops, \"Throughput\": $gp3_mibs, \"DeleteOnTermination\": true}}]" \
            --tag-specifications "ResourceType=instance,$tags" "ResourceType=volume,$tags" \
            "ResourceType=network-interface,$tags" > /dev/null 2> "$state/run-instances.err"; then
            break
        fi
        grep -q 'iamInstanceProfile' "$state/run-instances.err" || { cat "$state/run-instances.err" >&2; return 1; }
        ((attempt < 12)) || { cat "$state/run-instances.err" >&2; return 1; }
        sleep 5
    done
    "${cloud[@]}" ec2 wait instance-running "${owned[@]}"
}

# hosts prints each running host of the run, in launch order: its launch
# index, ID, private and public address, and its cache volume's ID.
hosts() {
    "${cloud[@]}" ec2 describe-instances "${owned[@]}" "Name=instance-state-name,Values=running" \
        --query "Reservations[].Instances[].[AmiLaunchIndex,InstanceId,PrivateIpAddress,PublicIpAddress,
            BlockDeviceMappings[?DeviceName=='/dev/sdf'].Ebs.VolumeId|[0]]" | sort -n
}

remote() {
    local address=$1
    shift
    # shellcheck disable=SC2029 # each command is built here to run there
    ssh "${ssh_options[@]}" "ec2-user@$address" "$@" < /dev/null
}

run() {
    local index id private public volume ready status=0 nodes="" run_objects
    local -a publics=() privates=() volumes=()
    run_objects="$objects/$(date -u +%Y%m%dT%H%M%SZ)"
    while read -r index id private public volume; do
        publics+=("$public")
        privates+=("$private")
        volumes+=("$volume")
    done < <(hosts)
    ((${#publics[@]} == count)) || { echo "Found ${#publics[@]} hosts of $run, want $count." >&2; return 1; }
    mkdir -p "$results"
    results=$(cd "$results" && pwd)
    for public in "${publics[@]}"; do
        ready=false
        for _ in {1..40}; do
            if remote "$public" 'test -e /var/lib/sproutfs-bench/ready' >> "$results/startup.log" 2>&1; then
                ready=true
                break
            fi
            sleep 10
        done
        "$ready" || { echo "$public did not finish starting." >&2; return 1; }
    done
    (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$state/sproutfs-restorebench" \
        ./cmd/sproutfs-restorebench)
    {
        echo "revision $(git -C "$repo" rev-parse HEAD)"
        git -C "$repo" status --porcelain=v1 -- cmd/sproutfs-restorebench checkpoint peer rank stripe platform \
            | sed 's/^/changed /'
        (cd "$state" && shasum -a 256 sproutfs-restorebench)
        echo "instance $instance_type in $zone_id, hosts $count, code $code, pages $pages, pages-4k $pages4k," \
            "reads $reads, rounds $rounds"
        echo "cache volumes gp3 ${gp3_gb} GiB, $gp3_iops IOPS, $gp3_mibs MiB/s"
        echo "regional s3://$regional_bucket/$run_objects"
        echo "hot tier s3://$express_bucket/$run_objects"
    } > "$results/source.txt"
    "${cloud[@]}" ec2 describe-instances "${owned[@]}" --output json --query \
        'Reservations[].Instances[].{id:InstanceId,type:InstanceType,zone:Placement.AvailabilityZone,
            ip:PrivateIpAddress,launch:AmiLaunchIndex,image:ImageId}' > "$results/instances.json"
    "${cloud[@]}" ec2 describe-volumes "${owned[@]}" --output json --query \
        'Volumes[].{id:VolumeId,type:VolumeType,size:Size,iops:Iops,throughput:Throughput,zone:AvailabilityZone}' \
        > "$results/volumes.json"
    for index in "${!publics[@]}"; do
        {
            remote "${publics[$index]}" "sudo systemctl stop sproutfs-node 2>/dev/null || true; \
                sudo systemctl reset-failed sproutfs-node 2>/dev/null || true; lscpu; ls -l /dev/disk/by-id/"
            scp "${ssh_options[@]}" "$state/sproutfs-restorebench" "$repo/scripts/lib/shard-fio.sh" \
                "ec2-user@${publics[$index]}:"
            remote "${publics[$index]}" "sudo systemd-run --unit=sproutfs-node --property=LimitNOFILE=65536 \
                --setenv=AWS_REGION=$region --setenv=GOMEMLIMIT=12GiB \
                /home/ec2-user/sproutfs-restorebench node -cloud aws -advertise ${privates[$index]}:7500 \
                -bucket $regional_bucket -hot-bucket $express_bucket -prefix $run_objects \
                -device ${volumes[$index]} -dir /var/tmp/sproutfs"
        } >> "$results/remote.log" 2>&1
        nodes+="${nodes:+,}${privates[$index]}:7600"
    done
    sleep 5
    echo "Walking from ${publics[1]}." >&2
    remote "${publics[1]}" "./sproutfs-restorebench walk -nodes $nodes -code $code -pages $pages \
        -pages-4k $pages4k -reads $reads -rounds $rounds -out walk.json" > "$results/walk.log" 2>&1 || status=$?
    scp "${ssh_options[@]}" "ec2-user@${publics[1]}:walk.json" "$results/walk.json" \
        >> "$results/remote.log" 2>&1 || status=$?
    for index in "${!publics[@]}"; do
        remote "${publics[$index]}" "sudo journalctl -u sproutfs-node --no-pager" \
            > "$results/node-$index.log" 2>&1 || status=$?
        remote "${publics[$index]}" "sudo systemctl stop sproutfs-node" >> "$results/remote.log" 2>&1 || true
    done
    echo "Measuring each cache volume with fio." >&2
    for index in "${!publics[@]}"; do
        remote "${publics[$index]}" "bash shard-fio.sh ${volumes[$index]} fio" \
            > "$results/fio-$index.log" 2>&1 &
    done
    wait || status=$?
    for index in "${!publics[@]}"; do
        mkdir -p "$results/fio-$index"
        scp -q "${ssh_options[@]}" "ec2-user@${publics[$index]}:fio/*.json" "$results/fio-$index/" \
            >> "$results/remote.log" 2>&1 || status=$?
    done
    return "$status"
}

# owned_ids prints the IDs of one kind of EC2 resource the run tagged.
owned_ids() {
    local kind=$1 query=$2
    shift 2
    "${cloud[@]}" ec2 "describe-$kind" "${owned[@]}" "$@" --query "$query"
}

delete() {
    local account ids id vpc bucket_tags
    account=$("${cloud[@]}" sts get-caller-identity --query Account)
    ids=$(owned_ids instances 'Reservations[].Instances[].InstanceId' \
        "Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down")
    if [[ -n $ids ]]; then
        # shellcheck disable=SC2086 # one ID a word
        "${cloud[@]}" ec2 terminate-instances --instance-ids $ids > /dev/null
        # shellcheck disable=SC2086
        "${cloud[@]}" ec2 wait instance-terminated --instance-ids $ids
    fi
    # Every volume was deleted with its instance; one left over is deleted.
    for _ in {1..30}; do
        ids=$(owned_ids volumes 'Volumes[].VolumeId')
        [[ -z $ids ]] && break
        for id in $(owned_ids volumes "Volumes[?State=='available'].VolumeId"); do
            "${cloud[@]}" ec2 delete-volume --volume-id "$id"
        done
        sleep 5
    done

    if "${cloud[@]}" s3api head-bucket --bucket "$regional_bucket" > /dev/null 2>&1; then
        bucket_tags=$("${cloud[@]}" s3api get-bucket-tagging --bucket "$regional_bucket" \
            --query "TagSet[?Key=='$tag_key'].Value" 2>/dev/null || true)
        if [[ $bucket_tags == "$run" ]]; then
            "${cloud[@]}" s3 rm --recursive --quiet "s3://$regional_bucket"
            "${cloud[@]}" s3api delete-bucket --bucket "$regional_bucket"
        else
            echo "Refusing to delete s3://$regional_bucket, which is not tagged $tag_key=$run." >&2
        fi
    fi
    if "${cloud[@]}" s3api list-directory-buckets --query "Buckets[].Name" | tr '\t' '\n' \
        | grep -qx -- "$express_bucket"; then
        bucket_tags=$("${cloud[@]}" s3control list-tags-for-resource --account-id "$account" \
            --resource-arn "arn:aws:s3express:$region:$account:bucket/$express_bucket" \
            --query "Tags[?Key=='$tag_key'].Value" 2>/dev/null || true)
        if [[ $bucket_tags == "$run" ]]; then
            "${cloud[@]}" s3 rm --recursive --quiet "s3://$express_bucket"
            "${cloud[@]}" s3api delete-bucket --bucket "$express_bucket"
        else
            echo "Refusing to delete s3://$express_bucket, which is not tagged $tag_key=$run." >&2
        fi
    fi

    for id in $(owned_ids key-pairs 'KeyPairs[].KeyPairId'); do
        "${cloud[@]}" ec2 delete-key-pair --key-pair-id "$id"
    done
    vpc=$(owned_ids vpcs 'Vpcs[].VpcId')
    for id in $(owned_ids security-groups 'SecurityGroups[].GroupId'); do
        "${cloud[@]}" ec2 delete-security-group --group-id "$id"
    done
    ids=$(owned_ids vpc-endpoints "VpcEndpoints[?State!='deleted'].VpcEndpointId")
    if [[ -n $ids ]]; then
        # shellcheck disable=SC2086
        "${cloud[@]}" ec2 delete-vpc-endpoints --vpc-endpoint-ids $ids > /dev/null
        for _ in {1..30}; do
            [[ -z $(owned_ids vpc-endpoints "VpcEndpoints[?State!='deleted'].VpcEndpointId") ]] && break
            sleep 5
        done
    fi
    for id in $(owned_ids route-tables 'RouteTables[].Associations[?!Main].RouteTableAssociationId[]'); do
        "${cloud[@]}" ec2 disassociate-route-table --association-id "$id"
    done
    for id in $(owned_ids route-tables 'RouteTables[].RouteTableId'); do
        "${cloud[@]}" ec2 delete-route-table --route-table-id "$id"
    done
    for id in $(owned_ids internet-gateways 'InternetGateways[].InternetGatewayId'); do
        [[ -n $vpc ]] && "${cloud[@]}" ec2 detach-internet-gateway --internet-gateway-id "$id" --vpc-id "$vpc" || true
        "${cloud[@]}" ec2 delete-internet-gateway --internet-gateway-id "$id"
    done
    for id in $(owned_ids subnets 'Subnets[].SubnetId'); do
        "${cloud[@]}" ec2 delete-subnet --subnet-id "$id"
    done
    for id in $vpc; do
        "${cloud[@]}" ec2 delete-vpc --vpc-id "$id"
    done

    if "${cloud[@]}" iam get-role --role-name "$run" > /dev/null 2>&1; then
        if [[ $("${cloud[@]}" iam list-role-tags --role-name "$run" --query "Tags[?Key=='$tag_key'].Value") == "$run" ]]; then
            "${cloud[@]}" iam remove-role-from-instance-profile --instance-profile-name "$run" --role-name "$run" \
                2>/dev/null || true
            "${cloud[@]}" iam delete-role-policy --role-name "$run" --policy-name buckets 2>/dev/null || true
            "${cloud[@]}" iam delete-role --role-name "$run"
        else
            echo "Refusing to delete the role $run, which is not tagged $tag_key=$run." >&2
        fi
    fi
    if "${cloud[@]}" iam get-instance-profile --instance-profile-name "$run" > /dev/null 2>&1; then
        if [[ $("${cloud[@]}" iam list-instance-profile-tags --instance-profile-name "$run" \
            --query "Tags[?Key=='$tag_key'].Value") == "$run" ]]; then
            "${cloud[@]}" iam delete-instance-profile --instance-profile-name "$run"
        else
            echo "Refusing to delete the instance profile $run, which is not tagged $tag_key=$run." >&2
        fi
    fi
    verify
}

# verify lists every kind of resource the run makes, and fails naming any
# that remains.
verify() {
    local left="" found kind
    for kind in "instances Reservations[].Instances[?State.Name!='terminated'].InstanceId[]" \
        "volumes Volumes[].VolumeId" "key-pairs KeyPairs[].KeyPairId" \
        "security-groups SecurityGroups[].GroupId" "vpc-endpoints VpcEndpoints[?State!='deleted'].VpcEndpointId" \
        "route-tables RouteTables[].RouteTableId" "internet-gateways InternetGateways[].InternetGatewayId" \
        "subnets Subnets[].SubnetId" "vpcs Vpcs[].VpcId"; do
        found=$(owned_ids "${kind%% *}" "${kind#* }")
        [[ -z $found ]] || left+=" ${kind%% *}: $found;"
    done
    if "${cloud[@]}" s3api head-bucket --bucket "$regional_bucket" > /dev/null 2>&1; then
        left+=" bucket s3://$regional_bucket;"
    fi
    if "${cloud[@]}" s3api list-directory-buckets --query "Buckets[].Name" | tr '\t' '\n' \
        | grep -qx -- "$express_bucket"; then
        left+=" directory bucket s3://$express_bucket;"
    fi
    if "${cloud[@]}" iam get-role --role-name "$run" > /dev/null 2>&1; then left+=" role $run;"; fi
    if "${cloud[@]}" iam get-instance-profile --instance-profile-name "$run" > /dev/null 2>&1; then
        left+=" instance profile $run;"
    fi
    [[ -z $left ]] || { echo "Still there:$left" >&2; return 1; }
    echo "Verified that nothing tagged $tag_key=$run remains: no instance, volume, key pair, security group," \
        "endpoint, route table, gateway, subnet or VPC, neither bucket, no role and no instance profile." >&2
}

case "$action" in
    create) create ;;
    run) run ;;
    delete) delete ;;
    verify) verify ;;
    all)
        [[ -z "$(owned_ids vpcs 'Vpcs[].VpcId')" ]] || { echo "A VPC tagged $tag_key=$run already exists." >&2; exit 1; }
        echo "Run $run; state in $state, results in $results." >&2
        trap 'status=$?; trap - EXIT; if ! delete; then exit 1; fi; exit "$status"' EXIT
        create
        run
        ;;
    *) echo "Usage: $0 [all|create|run|delete|verify] [run id] [results directory]" >&2; exit 2 ;;
esac
