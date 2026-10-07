package membership

import (
	"bytes"
	"errors"
	"fmt"

	membershipv1 "github.com/semistrict/sproutfs/membership/internal/gen/sproutfs/membership/v1"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
	"google.golang.org/protobuf/proto"
)

const (
	// formatVersion is the object's wire format. Version 2 gives a disk its
	// kind, the machine a journal disk is reserved for, and whether it is
	// empty.
	formatVersion = uint32(2)
	// maximumSize bounds the object: a few dozen bytes for each member and
	// each disk, so a megabyte is tens of thousands of hosts.
	maximumSize = int64(1 << 20)
	// nonceSize is the bytes of a writer's nonce.
	nonceSize = 16
)

// Marshal writes the membership as the object holds it.
func (m Membership) Marshal() ([]byte, error) {
	message := membershipv1.Membership_builder{FormatVersion: proto.Uint32(formatVersion),
		Generation: proto.Uint64(m.generation), K: proto.Uint32(uint32(m.code.K)), M: proto.Uint32(uint32(m.code.M)),
		WriterNonce: bytes.Clone(m.nonce)}.Build()
	members := make([]*membershipv1.Member, 0, len(m.members))
	for _, member := range m.members {
		state := membershipv1.MemberState(member.State)
		members = append(members, membershipv1.Member_builder{Id: bytes.Clone(member.ID[:]),
			Address: proto.String(string(member.Address)), State: &state}.Build())
	}
	disks := make([]*membershipv1.Disk, 0, len(m.disks))
	for _, disk := range m.disks {
		state := membershipv1.DiskState(disk.State)
		kind := membershipv1.DiskKind(disk.Kind + 1)
		built := membershipv1.Disk_builder{Id: bytes.Clone(disk.ID[:]), Volume: proto.String(disk.Volume),
			Weight: proto.Uint32(disk.Weight), State: &state, Assigned: proto.Uint64(disk.Assigned), Kind: &kind,
			Machine: proto.String(disk.Machine), Empty: proto.Bool(disk.Empty)}
		if !disk.Member.IsZero() {
			built.Member = bytes.Clone(disk.Member[:])
		}
		disks = append(disks, built.Build())
	}
	message.SetMembers(members)
	message.SetDisks(disks)
	earlier := make([]*membershipv1.Code, 0, len(m.earlier))
	for _, code := range m.earlier {
		earlier = append(earlier, membershipv1.Code_builder{K: proto.Uint32(uint32(code.K)),
			M: proto.Uint32(uint32(code.M))}.Build())
	}
	message.SetEarlier(earlier)
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximumSize {
		return nil, fmt.Errorf("%w: a membership of %d bytes, past %d", ErrInvalid, len(data), maximumSize)
	}
	return data, nil
}

// Unmarshal reads a membership as the object holds it, refusing one of
// another format, one with fields this build does not know, and one no host
// could route by.
func Unmarshal(data []byte) (Membership, error) {
	message := new(membershipv1.Membership)
	if err := proto.Unmarshal(data, message); err != nil {
		return Membership{}, errors.Join(ErrCorrupt, err)
	}
	if message.GetFormatVersion() != formatVersion {
		return Membership{}, fmt.Errorf("%w: format version %d, want %d", ErrCorrupt, message.GetFormatVersion(),
			formatVersion)
	}
	if len(message.ProtoReflect().GetUnknown()) != 0 || !message.HasGeneration() || !message.HasK() ||
		!message.HasM() || len(message.GetWriterNonce()) != nonceSize {
		return Membership{}, ErrCorrupt
	}
	members := make([]Member, 0, len(message.GetMembers()))
	for _, member := range message.GetMembers() {
		id, ok := identityOf(member.GetId())
		if len(member.ProtoReflect().GetUnknown()) != 0 || !ok || !member.HasAddress() || !member.HasState() {
			return Membership{}, ErrCorrupt
		}
		members = append(members, Member{ID: id, Address: platform.Address(member.GetAddress()),
			State: MemberState(member.GetState())})
	}
	disks := make([]Disk, 0, len(message.GetDisks()))
	for _, disk := range message.GetDisks() {
		id, ok := identityOf(disk.GetId())
		owner, owned := rank.Identity{}, true
		if member := disk.GetMember(); len(member) > 0 {
			owner, owned = identityOf(member)
		}
		if len(disk.ProtoReflect().GetUnknown()) != 0 || !ok || !owned || !disk.HasVolume() || !disk.HasWeight() ||
			!disk.HasState() || !disk.HasAssigned() || !disk.HasKind() || disk.GetKind() == 0 ||
			!disk.HasMachine() || !disk.HasEmpty() {
			return Membership{}, ErrCorrupt
		}
		disks = append(disks, Disk{ID: id, Volume: disk.GetVolume(), Weight: disk.GetWeight(), Member: owner,
			State: DiskState(disk.GetState()), Assigned: disk.GetAssigned(), Kind: DiskKind(disk.GetKind() - 1),
			Machine: disk.GetMachine(), Empty: disk.GetEmpty()})
	}
	earlier := make([]rank.Code, 0, len(message.GetEarlier()))
	for _, code := range message.GetEarlier() {
		if len(code.ProtoReflect().GetUnknown()) != 0 || !code.HasK() || !code.HasM() {
			return Membership{}, ErrCorrupt
		}
		earlier = append(earlier, rank.Code{K: int(code.GetK()), M: int(code.GetM())})
	}
	m, err := New(message.GetGeneration(), rank.Code{K: int(message.GetK()), M: int(message.GetM())}, members, disks,
		earlier...)
	if err != nil {
		return Membership{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	m.nonce = bytes.Clone(message.GetWriterNonce())
	return m, nil
}

// identityOf reads an identity off the wire.
func identityOf(raw []byte) (rank.Identity, bool) {
	var id rank.Identity
	if len(raw) != len(id) {
		return rank.Identity{}, false
	}
	copy(id[:], raw)
	return id, true
}
