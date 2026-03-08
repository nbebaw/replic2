// Package types contains the Go struct definitions for every CRD that
// replic2 owns: Backup, Restore, and ScheduledBackup.
//
// Group:   replic2.io
// Version: v1alpha1
//
// All types implement runtime.Object so the API machinery can decode raw
// API-server responses directly into these structs.
package types

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// SchemeGroupVersion is the canonical GVK for replic2 CRDs.
var SchemeGroupVersion = schema.GroupVersion{
	Group:   "replic2.io",
	Version: "v1alpha1",
}

// AddToScheme registers all replic2 CRD types with the given runtime.Scheme.
// Call this once at startup (see internal/k8s).
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion,
		&Backup{},
		&BackupList{},
		&Restore{},
		&RestoreList{},
		&ScheduledBackup{},
		&ScheduledBackupList{},
	)
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}

// ---------------------------------------------------------------------------
// Phase constants — used across controllers so callers never spell-check strings.
// ---------------------------------------------------------------------------

const (
	PhasePending    = "Pending"
	PhaseInProgress = "InProgress"
	PhaseCompleted  = "Completed"
	PhaseFailed     = "Failed"
)

// ---------------------------------------------------------------------------
// Backup type constants
// ---------------------------------------------------------------------------

const (
	// BackupTypeFull captures all resources and PVC data from scratch.
	BackupTypeFull = "Full"
	// BackupTypeIncremental captures only resources/files that changed since
	// the most recent completed backup for the same namespace.
	BackupTypeIncremental = "Incremental"
)

// ---------------------------------------------------------------------------
// Backup
// ---------------------------------------------------------------------------

// BackupSpec defines what to back up.
type BackupSpec struct {
	// Namespace is the Kubernetes namespace to back up.
	Namespace string `json:"namespace"`
	// TTL is an optional retention duration (e.g. "24h").
	// When set, the backup controller deletes the CR and its PVC data after
	// completedAt + TTL has elapsed.
	TTL string `json:"ttl,omitempty"`
	// Type is either "Full" or "Incremental".
	// When empty the controller auto-selects: Full if no prior completed
	// backup exists for the namespace, Incremental otherwise.
	Type string `json:"type,omitempty"`
	// IncludePVCData when true copies the raw data from every PVC bound in
	// the target namespace in addition to the Kubernetes manifests.
	IncludePVCData bool `json:"includePVCData,omitempty"`
}

// BackupStatus is written back by the backup controller.
type BackupStatus struct {
	// Phase is one of: Pending | InProgress | Completed | Failed.
	Phase string `json:"phase,omitempty"`
	// Message is a human-readable status string.
	Message string `json:"message,omitempty"`
	// StartedAt is when the backup began.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the backup finished (success or failure).
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// StoragePath is the directory on the PVC where YAML files were written.
	StoragePath string `json:"storagePath,omitempty"`
	// BasedOn is the name of the Backup CR that this incremental backup is
	// built on top of.  Empty for full backups.
	BasedOn string `json:"basedOn,omitempty"`
	// BackupType records whether this was a "Full" or "Incremental" backup.
	BackupType string `json:"backupType,omitempty"`
}

// Backup is the Schema for the backups.replic2.io CRD.
type Backup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupSpec   `json:"spec"`
	Status BackupStatus `json:"status,omitempty"`
}

// BackupList is the list wrapper required by the API machinery.
type BackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Backup `json:"items"`
}

// DeepCopyObject implements runtime.Object for Backup.
func (b *Backup) DeepCopyObject() runtime.Object {
	out := new(Backup)
	out.TypeMeta = b.TypeMeta
	out.ObjectMeta = *b.ObjectMeta.DeepCopy()
	out.Spec = b.Spec
	out.Status = b.Status
	if b.Status.StartedAt != nil {
		t := *b.Status.StartedAt
		out.Status.StartedAt = &t
	}
	if b.Status.CompletedAt != nil {
		t := *b.Status.CompletedAt
		out.Status.CompletedAt = &t
	}
	return out
}

// DeepCopyObject implements runtime.Object for BackupList.
func (bl *BackupList) DeepCopyObject() runtime.Object {
	out := new(BackupList)
	out.TypeMeta = bl.TypeMeta
	out.ListMeta = bl.ListMeta
	if bl.Items != nil {
		out.Items = make([]Backup, len(bl.Items))
		for i := range bl.Items {
			item := *bl.Items[i].DeepCopyObject().(*Backup)
			out.Items[i] = item
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

// RestoreSpec defines which namespace to restore and which backup to use.
type RestoreSpec struct {
	// Namespace is the Kubernetes namespace to restore into.
	Namespace string `json:"namespace"`
	// BackupName is the Backup CR to restore from.
	// If empty, the controller picks the most recently completed backup for
	// the given namespace.
	BackupName string `json:"backupName,omitempty"`
}

// RestoreStatus is written back by the restore controller.
type RestoreStatus struct {
	// Phase is one of: Pending | InProgress | Completed | Failed.
	Phase string `json:"phase,omitempty"`
	// Message is a human-readable status string.
	Message string `json:"message,omitempty"`
	// StartedAt is when the restore began.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the restore finished.
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// RestoredFrom is the PVC storage path that was read.
	RestoredFrom string `json:"restoredFrom,omitempty"`
}

// Restore is the Schema for the restores.replic2.io CRD.
type Restore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RestoreSpec   `json:"spec"`
	Status RestoreStatus `json:"status,omitempty"`
}

// RestoreList is the list wrapper required by the API machinery.
type RestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Restore `json:"items"`
}

// DeepCopyObject implements runtime.Object for Restore.
func (r *Restore) DeepCopyObject() runtime.Object {
	out := new(Restore)
	out.TypeMeta = r.TypeMeta
	out.ObjectMeta = *r.ObjectMeta.DeepCopy()
	out.Spec = r.Spec
	out.Status = r.Status
	if r.Status.StartedAt != nil {
		t := *r.Status.StartedAt
		out.Status.StartedAt = &t
	}
	if r.Status.CompletedAt != nil {
		t := *r.Status.CompletedAt
		out.Status.CompletedAt = &t
	}
	return out
}

// DeepCopyObject implements runtime.Object for RestoreList.
func (rl *RestoreList) DeepCopyObject() runtime.Object {
	out := new(RestoreList)
	out.TypeMeta = rl.TypeMeta
	out.ListMeta = rl.ListMeta
	if rl.Items != nil {
		out.Items = make([]Restore, len(rl.Items))
		for i := range rl.Items {
			item := *rl.Items[i].DeepCopyObject().(*Restore)
			out.Items[i] = item
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// ScheduledBackup
// ---------------------------------------------------------------------------

// ScheduledBackupSpec defines the cron schedule and retention policy.
type ScheduledBackupSpec struct {
	// Namespace is the Kubernetes namespace to back up on each run.
	Namespace string `json:"namespace"`
	// Schedule is a standard 5-field cron expression (UTC), e.g. "0 2 * * *".
	Schedule string `json:"schedule"`
	// KeepLast is the number of most-recent Backup CRs to retain.
	// Older ones are deleted automatically. 0 means keep all.
	KeepLast int `json:"keepLast,omitempty"`
	// TTL is an optional Go duration (e.g. "24h") stamped on every generated
	// Backup CR so the TTL controller can also prune them.
	TTL string `json:"ttl,omitempty"`
}

// ScheduledBackupStatus is written back by the scheduled-backup controller.
type ScheduledBackupStatus struct {
	// LastScheduleTime is when the most recent Backup CR was created.
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// LastBackupName is the name of the most recently created Backup CR.
	LastBackupName string `json:"lastBackupName,omitempty"`
	// ActiveBackups is the count of Backup CRs currently owned by this schedule.
	ActiveBackups int `json:"activeBackups,omitempty"`
	// Message is a human-readable status string.
	Message string `json:"message,omitempty"`
}

// ScheduledBackup is the Schema for the scheduledbackups.replic2.io CRD.
type ScheduledBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScheduledBackupSpec   `json:"spec"`
	Status ScheduledBackupStatus `json:"status,omitempty"`
}

// ScheduledBackupList is the list wrapper required by the API machinery.
type ScheduledBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ScheduledBackup `json:"items"`
}

// DeepCopyObject implements runtime.Object for ScheduledBackup.
func (sb *ScheduledBackup) DeepCopyObject() runtime.Object {
	out := new(ScheduledBackup)
	out.TypeMeta = sb.TypeMeta
	out.ObjectMeta = *sb.ObjectMeta.DeepCopy()
	out.Spec = sb.Spec
	out.Status = sb.Status
	if sb.Status.LastScheduleTime != nil {
		t := *sb.Status.LastScheduleTime
		out.Status.LastScheduleTime = &t
	}
	return out
}

// DeepCopyObject implements runtime.Object for ScheduledBackupList.
func (sbl *ScheduledBackupList) DeepCopyObject() runtime.Object {
	out := new(ScheduledBackupList)
	out.TypeMeta = sbl.TypeMeta
	out.ListMeta = sbl.ListMeta
	if sbl.Items != nil {
		out.Items = make([]ScheduledBackup, len(sbl.Items))
		for i := range sbl.Items {
			item := *sbl.Items[i].DeepCopyObject().(*ScheduledBackup)
			out.Items[i] = item
		}
	}
	return out
}
