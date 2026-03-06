package main

// types.go — Go struct definitions for the Backup and Restore CRDs.
//
// Group:   replic2.io
// Version: v1alpha1
//
// These types are registered with the API machinery so that the dynamic
// informer machinery can decode objects from the API server into typed structs.

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// -----------------------------------------------------------------------
// Scheme registration
// -----------------------------------------------------------------------

// SchemeGroupVersion is the canonical GVK for our CRDs.
var SchemeGroupVersion = schema.GroupVersion{
	Group:   "replic2.io",
	Version: "v1alpha1",
}

// addToScheme registers Backup and Restore with the given runtime.Scheme.
func addToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion,
		&Backup{},
		&BackupList{},
		&Restore{},
		&RestoreList{},
	)
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}

// -----------------------------------------------------------------------
// Backup CRD types
// -----------------------------------------------------------------------

// BackupSpec defines what should be backed up.
type BackupSpec struct {
	// Namespace is the Kubernetes namespace to back up.
	Namespace string `json:"namespace"`
	// TTL is an optional retention hint (e.g. "24h"). Not yet enforced.
	TTL string `json:"ttl,omitempty"`
}

// BackupStatus is written back by the controller.
type BackupStatus struct {
	// Phase is one of Pending | InProgress | Completed | Failed.
	Phase string `json:"phase,omitempty"`
	// Message is a human-readable reason string.
	Message string `json:"message,omitempty"`
	// StartedAt is when the backup began.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the backup finished.
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// StoragePath is the directory on the PVC where files were written.
	StoragePath string `json:"storagePath,omitempty"`
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
	*out = *b
	out.TypeMeta = b.TypeMeta
	out.ObjectMeta = *b.ObjectMeta.DeepCopy()
	out.Spec = b.Spec
	if b.Status.StartedAt != nil {
		t := *b.Status.StartedAt
		out.Status.StartedAt = &t
	}
	if b.Status.CompletedAt != nil {
		t := *b.Status.CompletedAt
		out.Status.CompletedAt = &t
	}
	out.Status.Phase = b.Status.Phase
	out.Status.Message = b.Status.Message
	out.Status.StoragePath = b.Status.StoragePath
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
			bl.Items[i].DeepCopyObject() // validate; we copy below
			item := *bl.Items[i].DeepCopyObject().(*Backup)
			out.Items[i] = item
		}
	}
	return out
}

// -----------------------------------------------------------------------
// Restore CRD types
// -----------------------------------------------------------------------

// RestoreSpec defines which namespace to restore and optionally which backup.
type RestoreSpec struct {
	// Namespace is the Kubernetes namespace to restore into.
	Namespace string `json:"namespace"`
	// BackupName is the name of the Backup CR to restore from.
	// If empty, the controller selects the most recent Completed backup for
	// the given namespace.
	BackupName string `json:"backupName,omitempty"`
}

// RestoreStatus is written back by the controller.
type RestoreStatus struct {
	// Phase is one of Pending | InProgress | Completed | Failed.
	Phase string `json:"phase,omitempty"`
	// Message is a human-readable reason string.
	Message string `json:"message,omitempty"`
	// StartedAt is when the restore began.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the restore finished.
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// RestoredFrom is the storage path that was read.
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
	*out = *r
	out.TypeMeta = r.TypeMeta
	out.ObjectMeta = *r.ObjectMeta.DeepCopy()
	out.Spec = r.Spec
	if r.Status.StartedAt != nil {
		t := *r.Status.StartedAt
		out.Status.StartedAt = &t
	}
	if r.Status.CompletedAt != nil {
		t := *r.Status.CompletedAt
		out.Status.CompletedAt = &t
	}
	out.Status.Phase = r.Status.Phase
	out.Status.Message = r.Status.Message
	out.Status.RestoredFrom = r.Status.RestoredFrom
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
