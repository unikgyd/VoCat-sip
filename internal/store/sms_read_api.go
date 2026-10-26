package store

import (
	"context"
	"strconv"
	"time"

	"vocat/internal/smsquery"
)

// QueryReadOnlySMS deliberately selects no IMSI, ICCID, raw PDU, read state or
// device configuration. slotId means the stored original device_id, not a
// lookup against whichever SIM is currently installed. This method only SELECTs.
func (s *Store) QueryReadOnlySMS(ctx context.Context, q smsquery.Query) (smsquery.Result, error) {
	result := smsquery.Result{Messages: make([]smsquery.Message, 0)}
	if q.Limit < 1 || q.Limit > 100 || q.Since.After(q.Until) {
		return result, smsquery.ErrQuery
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,device_id,peer,body,message_time FROM sms_messages
	 WHERE direction IN ('inbound','received') AND message_time>=? AND message_time<=?
	 AND (?='' OR peer=?) AND (?='' OR device_id=?) ORDER BY message_time DESC,id DESC`,
		q.Since.Unix(), q.Until.Unix(), q.Sender, q.Sender, q.SlotID, q.SlotID)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	scanned := 0
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		var m smsquery.Message
		var id, timestamp int64
		var slot string
		if err = rows.Scan(&id, &slot, &m.Sender, &m.Text, &timestamp); err != nil {
			return result, err
		}
		m.Timestamp = time.Unix(timestamp, 0).UTC()
		if m.Timestamp.Before(q.Since) || !smsquery.Contains(m.Text, q.Keyword) {
			continue
		}
		scanned++
		m.ID = strconv.FormatInt(id, 10)
		if slot != "" {
			m.SlotID = &slot
		}
		m.Code = smsquery.ExtractCode(m.Text)
		if !q.OTPOnly || m.Code != nil {
			m.Text, m.TextTruncated = smsquery.Truncate(m.Text)
			result.Messages = append(result.Messages, m)
		}
		if len(result.Messages) >= q.Limit {
			break
		}
		if q.OTPOnly && scanned >= 1000 {
			result.ScanLimited = true
			break
		}
	}
	return result, rows.Err()
}
